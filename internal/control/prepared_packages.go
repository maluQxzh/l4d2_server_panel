package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func reservePreparedJobSlot(ctx context.Context, tx *sql.Tx, nodeID, kind string) error {
	var count, bundles int
	err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(json_extract(p.payload,'$.kind')='qq_vpk_bundle'),0) FROM jobs p WHERE p.node_id=?1 AND p.type='prepare_package'
		AND (p.status IN ('queued','dispatched','running') OR (p.status='canceled' AND json_extract(p.result,'$.cancel_pending')=1)
		OR (p.status='succeeded' AND p.created_at>?2 AND NOT EXISTS(SELECT 1 FROM jobs c WHERE c.type IN ('install_mod','discard_prepared_package') AND json_type(c.payload,'$.prepared_package_id')='text' AND json_extract(c.payload,'$.prepared_package_id')=p.id)))`, nodeID, time.Now().Add(-2*time.Hour)).Scan(&count, &bundles)
	if err != nil {
		return err
	}
	if count >= domain.PreparedPackageSlots {
		return fmt.Errorf("节点预下载队列已满，请完成或丢弃已有包: %w", ErrConflict)
	}
	if kind == domain.PreparedVPKBundleKind && bundles >= domain.PreparedVPKBundleSlots {
		return fmt.Errorf("节点整包预下载队列已满，请完成或丢弃已有文件包: %w", ErrConflict)
	}
	return nil
}

func preparedMetadata(job domain.Job) (domain.PreparedPackage, error) {
	var item domain.PreparedPackage
	raw, err := json.Marshal(job.Result)
	if err == nil {
		err = json.Unmarshal(raw, &item)
	}
	limit := domain.PreparedPackageMaxBytes
	if item.Kind == domain.PreparedVPKBundleKind {
		limit = domain.PreparedVPKBundleMaxBytes
	}
	if err != nil || job.Type != "prepare_package" || job.Status != domain.JobSucceeded ||
		item.ID != job.ID || item.InstanceID != job.InstanceID || item.NodeID != job.NodeID ||
		(stringFromAny(job.Payload["source_transport"]) != "qq_flash" && item.Name != stringFromAny(job.Payload["name"])) || item.Kind != stringFromAny(job.Payload["kind"]) ||
		!validRequestFingerprint(item.SHA256) || item.Size <= 0 || item.Size > limit ||
		item.ExpiresAt.IsZero() || item.ExpiresAt.After(job.CreatedAt.Add(2*time.Hour)) {
		return item, fmt.Errorf("预下载结果无效或未完成: %w", ErrConflict)
	}
	if item.Kind == domain.PreparedVPKBundleKind {
		var bundle domain.PreparedVPKBundle
		if json.Unmarshal(raw, &bundle) != nil || bundle.Validate() != nil || stringFromAny(job.Payload["source_transport"]) != "qq_flash" {
			return item, fmt.Errorf("预下载文件包清单无效: %w", ErrConflict)
		}
		return item, nil
	}
	_, err = domain.PreparedPackageTarget(item.Kind, item.Name)
	return item, err
}

// Called inside the immediate queue transaction, after idempotent replay and
// before insertion. No browser-supplied path, hash or size becomes authoritative.
func preparedConsumptionPayload(ctx context.Context, tx *sql.Tx, instanceID, nodeID, action string, payload map[string]any) (map[string]any, error) {
	return preparedConsumptionPayloadForQueue(ctx, tx, instanceID, nodeID, action, payload, "", "")
}

func preparedConsumptionPayloadForQueue(ctx context.Context, tx *sql.Tx, instanceID, nodeID, action string, payload map[string]any, queueID, claimID string) (map[string]any, error) {
	if action != "install_mod" && action != "discard_prepared_package" {
		return nil, ErrConflict
	}
	id := stringFromAny(payload["prepared_package_id"])
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1 AND instance_id=?2 AND node_id=?3 AND type='prepare_package'`, id, instanceID, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var claimed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE type IN ('install_mod','discard_prepared_package') AND json_type(payload,'$.prepared_package_id')='text' AND json_extract(payload,'$.prepared_package_id')=?1 AND id<>?2) OR EXISTS(SELECT 1 FROM prepared_install_queue_items WHERE prepared_id=?1 AND state='waiting' AND queue_id<>?3)`, id, claimID, queueID).Scan(&claimed); err != nil {
		return nil, err
	}
	if claimed {
		return nil, fmt.Errorf("预下载包已提交安装或丢弃: %w", ErrConflict)
	}
	item, err := preparedMetadata(job)
	if err != nil {
		return nil, err
	}
	if !item.ExpiresAt.After(time.Now()) && action != "discard_prepared_package" {
		return nil, fmt.Errorf("预下载包已过期，请重新准备: %w", ErrConflict)
	}
	risk, _ := payload["acknowledge_native_risk"].(bool)
	if action == "install_mod" && item.Kind == "native" && !risk {
		return nil, errors.New("原生扩展安装必须确认第三方代码风险")
	}
	if action == "install_mod" && item.Kind == domain.PreparedVPKBundleKind {
		var bundle domain.PreparedVPKBundle
		raw, _ := json.Marshal(job.Result)
		_ = json.Unmarshal(raw, &bundle) // Already fully validated by preparedMetadata.
		targets := make([]string, 0, len(bundle.Files))
		for index, file := range bundle.Files {
			target, _ := domain.PreparedVPKBundleTarget(item.ID, index, file.Name)
			targets = append(targets, target)
		}
		rawTargets, _ := json.Marshal(targets)
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM addon_package_files WHERE instance_id=?1 AND lower(path) IN (SELECT lower(value) FROM json_each(?2))) OR EXISTS(SELECT 1 FROM addon_packages WHERE instance_id=?1 AND lower(path) IN (SELECT lower(value) FROM json_each(?2)))`, instanceID, string(rawTargets)).Scan(&owned); err != nil {
			return nil, err
		}
		if owned {
			return nil, fmt.Errorf("文件包目标已由其他包管理: %w", ErrConflict)
		}
	} else if action == "install_mod" {
		target, err := domain.PreparedPackageTarget(item.Kind, item.Name)
		if err != nil {
			return nil, err
		}
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM addon_package_files WHERE instance_id=?1 AND lower(path)=lower(?2))`, instanceID, target).Scan(&owned); err != nil {
			return nil, err
		}
		if owned {
			return nil, fmt.Errorf("该文件已由整包插件或组件管理，请通过原包更新: %w", ErrConflict)
		}
	}
	return map[string]any{"prepared_package_id": item.ID, "name": item.Name, "kind": item.Kind,
		"expected_sha256": item.SHA256, "expected_size": item.Size, "risk_confirmed": risk, "acknowledge_native_risk": risk}, nil
}

func (s *Server) handlePreparePackage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Source    string `json:"source"`
		Content   string `json:"content_base64"`
		Transport string `json:"source_transport"`
	}
	if !decodeJSONWithLimit(w, r, &request, protocol.MaxPackageUploadEnvelopeBytes) {
		return
	}
	if len(request.Source) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_source", "HTTPS 地址过长")
		return
	}
	request.Kind = strings.ToLower(strings.TrimSpace(request.Kind))
	if request.Transport != "" && request.Transport != "qq_flash" {
		writeError(w, http.StatusBadRequest, "invalid_source", "不支持的预下载传输方式")
		return
	}
	if request.Transport == "qq_flash" {
		if (!domain.ValidPreparedPackageKind(request.Kind) && request.Kind != domain.PreparedVPKBundleKind) || request.Name != "" || request.Content != "" {
			writeError(w, http.StatusBadRequest, "invalid_package", "QQ 闪传预下载仅接受支持的类型和分享链接")
			return
		}
		var err error
		request.Source, err = normalizeQQFlashShareURL(request.Source)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_source", "请输入有效的 QQ 闪传分享链接")
			return
		}
	}
	instanceID := chi.URLParam(r, "instanceID")
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1) {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "请确保节点在线并升级 Agent 后使用预下载")
		return
	}
	if request.Transport == "qq_flash" && !s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedQQFlashV1) {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "请升级 Agent 后使用 QQ 闪传预下载")
		return
	}
	if request.Kind == domain.PreparedVPKBundleKind && !s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedVPKBundlesV1) {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "请升级 Agent 后使用 QQ 闪传整包预下载")
		return
	}
	if request.Source != "" && request.Transport == "" {
		u, err := url.Parse(request.Source)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			writeError(w, http.StatusBadRequest, "invalid_source", "预下载仅接受 HTTPS 地址")
			return
		}
		if request.Name == "" {
			request.Name = path.Base(u.Path)
		}
	}
	request.Kind, request.Name = strings.ToLower(strings.TrimSpace(request.Kind)), strings.TrimSpace(request.Name)
	if request.Transport == "" {
		if _, err := domain.PreparedPackageTarget(request.Kind, request.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_package", err.Error())
			return
		}
	}
	payload := map[string]any{"kind": request.Kind, "name": request.Name, "source": request.Source, "content_base64": request.Content}
	if (request.Source == "") == (request.Content == "") {
		writeError(w, http.StatusBadRequest, "invalid_package", "请选择一个上传文件或 HTTPS 来源")
		return
	}
	if err := validatePackageUpload(payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_package", err.Error())
		return
	}
	if request.Transport != "" {
		payload["source_transport"] = request.Transport
	}
	principal := principalFromContext(r.Context())
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "package-prepare", instanceID, "prepare_package", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "prepare_package", payload); err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "prepare_package", payload, principal.User.ID, r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.preparedJobAccepted(w, r, job, reused)
}

func (s *Server) handleConsumePreparedPackage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Risk bool `json:"acknowledge_native_risk"`
	}
	action := "install_mod"
	if r.Method == http.MethodDelete {
		action = "discard_prepared_package"
	} else if !decodeJSON(w, r, &request) {
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1) {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 不支持预下载包，请先升级")
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedVPKBundlesV1) {
		prepared, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
		if err == nil && prepared.InstanceID == instanceID && stringFromAny(prepared.Payload["kind"]) == domain.PreparedVPKBundleKind {
			writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 不支持整包预下载，请先升级")
			return
		}
	}
	payload := map[string]any{"prepared_package_id": chi.URLParam(r, "jobID"), "acknowledge_native_risk": request.Risk}
	principal := principalFromContext(r.Context())
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "prepared-package-consume", instanceID, action, payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, action, payload, principal.User.ID, r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_prepared_package", err.Error())
		}
		return
	}
	s.preparedJobAccepted(w, r, job, reused)
}

func (s *Server) preparedJobAccepted(w http.ResponseWriter, r *http.Request, job domain.Job, reused bool) {
	s.audit(r, job.Type+".requested", "instance", job.InstanceID, map[string]any{"job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handlePreparedPackages(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	// Polling only needs the public metadata, never a potentially 100 MiB
	// encrypted upload body. Project before transferring rows into Go.
	columns := strings.Replace(jobColumns, ",payload,", ",json_object('name',json_extract(payload,'$.name'),'kind',json_extract(payload,'$.kind'),'source_transport',json_extract(payload,'$.source_transport')),", 1)
	rows, err := s.store.db.QueryContext(r.Context(), `SELECT `+columns+` FROM jobs WHERE instance_id=?1 AND type='prepare_package'
		AND created_at>?2 ORDER BY created_at DESC,id DESC LIMIT 64`, instance.ID, time.Now().Add(-2*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	jobs := []domain.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			s.fail(w, err)
			return
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.fail(w, err)
		return
	}
	items := []map[string]any{}
	for _, job := range jobs {
		item := map[string]any{"id": job.ID, "name": stringFromAny(job.Payload["name"]), "kind": stringFromAny(job.Payload["kind"]), "status": job.Status, "progress": job.Progress}
		if item["kind"] == domain.PreparedVPKBundleKind {
			item["name"] = domain.PreparedVPKBundleName
		}
		if meta, err := preparedMetadata(job); err == nil {
			item["name"] = meta.Name
			item["expires_at"] = meta.ExpiresAt
			item["ready"] = meta.ExpiresAt.After(time.Now())
			item["size"] = meta.Size
			if meta.Kind == domain.PreparedVPKBundleKind {
				if files, ok := job.Result["files"].([]any); ok {
					item["file_count"] = len(files)
				}
			}
		}
		var claimID, status string
		var busy bool
		err := s.store.db.QueryRowContext(r.Context(), `SELECT id,status,holds_instance_write_lock FROM jobs WHERE type IN ('install_mod','discard_prepared_package') AND json_type(payload,'$.prepared_package_id')='text' AND json_extract(payload,'$.prepared_package_id')=?1`, job.ID).Scan(&claimID, &status, &busy)
		if err == nil {
			item["ready"] = false
			item["consume_job_id"] = claimID
			item["consume_status"] = status
			item["busy"] = busy
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.fail(w, err)
			return
		}
		var queueID string
		err = s.store.db.QueryRowContext(r.Context(), `SELECT queue_id FROM prepared_install_queue_items WHERE prepared_id=?1 AND state='waiting'`, job.ID).Scan(&queueID)
		if err == nil {
			item["ready"] = false
			item["queue_id"] = queueID
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.fail(w, err)
			return
		}
		items = append(items, item)
	}
	queueRows, err := s.store.db.QueryContext(r.Context(), `SELECT j.id,j.status,j.progress,j.message FROM prepared_install_queues q JOIN jobs j ON j.id=q.job_id WHERE q.instance_id=?1 ORDER BY q.active DESC,j.created_at DESC,j.id DESC LIMIT 10`, instance.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	queues := []map[string]any{}
	for queueRows.Next() {
		var id, status, message string
		var progress int
		if err = queueRows.Scan(&id, &status, &progress, &message); err != nil {
			break
		}
		queues = append(queues, map[string]any{"id": id, "status": status, "progress": progress, "message": message})
	}
	if err == nil {
		err = queueRows.Err()
	}
	queueRows.Close()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "queues": queues, "supported": s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1), "vpk_bundles_supported": s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1) && s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedQQFlashV1) && s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedVPKBundlesV1), "qq_flash_supported": s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1) && s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedQQFlashV1)})
}
