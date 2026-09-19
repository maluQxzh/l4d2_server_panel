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

func reservePreparedJobSlot(ctx context.Context, tx *sql.Tx, nodeID string) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs p WHERE p.node_id=?1 AND p.type='prepare_package'
		AND (p.status IN ('queued','dispatched','running') OR (p.status='canceled' AND json_extract(p.result,'$.cancel_pending')=1)
		OR (p.status='succeeded' AND p.created_at>?2 AND NOT EXISTS(SELECT 1 FROM jobs c WHERE c.type IN ('install_mod','discard_prepared_package') AND json_type(c.payload,'$.prepared_package_id')='text' AND json_extract(c.payload,'$.prepared_package_id')=p.id)))`, nodeID, time.Now().Add(-2*time.Hour)).Scan(&count)
	if err != nil {
		return err
	}
	if count >= domain.PreparedPackageSlots {
		return fmt.Errorf("节点预下载队列已满，请完成或丢弃已有包: %w", ErrConflict)
	}
	return nil
}

func preparedMetadata(job domain.Job) (domain.PreparedPackage, error) {
	var item domain.PreparedPackage
	raw, err := json.Marshal(job.Result)
	if err == nil {
		err = json.Unmarshal(raw, &item)
	}
	if err != nil || job.Type != "prepare_package" || job.Status != domain.JobSucceeded ||
		item.ID != job.ID || item.InstanceID != job.InstanceID || item.NodeID != job.NodeID ||
		item.Name != stringFromAny(job.Payload["name"]) || item.Kind != stringFromAny(job.Payload["kind"]) ||
		!validRequestFingerprint(item.SHA256) || item.Size <= 0 || item.Size > domain.PreparedPackageMaxBytes ||
		item.ExpiresAt.IsZero() || item.ExpiresAt.After(job.CreatedAt.Add(2*time.Hour)) {
		return item, fmt.Errorf("预下载结果无效或未完成: %w", ErrConflict)
	}
	_, err = domain.PreparedPackageTarget(item.Kind, item.Name)
	return item, err
}

// Called inside the immediate queue transaction, after idempotent replay and
// before insertion. No browser-supplied path, hash or size becomes authoritative.
func preparedConsumptionPayload(ctx context.Context, tx *sql.Tx, instanceID, nodeID, action string, payload map[string]any) (map[string]any, error) {
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
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE type IN ('install_mod','discard_prepared_package') AND json_type(payload,'$.prepared_package_id')='text' AND json_extract(payload,'$.prepared_package_id')=?1)`, id).Scan(&claimed); err != nil {
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
	if action == "install_mod" {
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
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Source  string `json:"source"`
		Content string `json:"content_base64"`
	}
	if !decodeJSONWithLimit(w, r, &request, protocol.MaxPackageUploadEnvelopeBytes) {
		return
	}
	if len(request.Source) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_source", "HTTPS 地址过长")
		return
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
	if request.Source != "" {
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
	if _, err := domain.PreparedPackageTarget(request.Kind, request.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_package", err.Error())
		return
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
	columns := strings.Replace(jobColumns, ",payload,", ",json_object('name',json_extract(payload,'$.name'),'kind',json_extract(payload,'$.kind')),", 1)
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
		if meta, err := preparedMetadata(job); err == nil {
			item["expires_at"] = meta.ExpiresAt
			item["ready"] = meta.ExpiresAt.After(time.Now())
			item["size"] = meta.Size
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
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "supported": s.agents.Supports(instance.NodeID, protocol.CapabilityPreparedPackagesV1)})
}
