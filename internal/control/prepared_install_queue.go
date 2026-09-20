package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

const preparedInstallQueueType = "prepared_install_queue"

type preparedInstallQueueRequest struct {
	IDs  []string `json:"prepared_ids"`
	Risk bool     `json:"acknowledge_native_risk"`
}

// A session delegates durable work to its user; closing the browser or logging
// out does not revoke that delegation. Tokens additionally retain their exact
// identity and current grants. All reads run in the same immediate transaction
// as acceptance, child creation, or the first dispatch claim.
func authorizePreparedInstallQueueTx(ctx context.Context, tx *sql.Tx, userID, tokenID, instanceID string) error {
	var nodeID string
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT i.node_id,EXISTS(
		SELECT 1 FROM users u JOIN role_bindings b ON b.user_id=u.id
		JOIN role_binding_permissions p ON p.binding_id=b.id
		WHERE u.id=?2 AND NOT u.disabled AND u.account_state='active' AND p.permission='packages.install'
		AND (b.scope_type='global' OR (b.scope_type='node' AND b.node_id=i.node_id) OR (b.scope_type='instance' AND b.instance_id=i.id)))
		FROM instances i JOIN nodes n ON n.id=i.node_id WHERE i.id=?1 AND n.status<>'revoked'`, instanceID, userID).Scan(&nodeID, &allowed)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !allowed) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if tokenID == "" {
		return nil
	}
	var mask int64
	var tokenNode, tokenInstance string
	err = tx.QueryRowContext(ctx, `SELECT scope_mask,COALESCE(node_id,''),COALESCE(instance_id,'') FROM api_tokens
		WHERE id=?1 AND user_id=?2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?3)`, tokenID, userID, time.Now().UTC()).Scan(&mask, &tokenNode, &tokenInstance)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if !tokenScopesAllow(tokenScopesFromMask(mask), PermissionPackagesInstall) || (tokenNode != "" && tokenNode != nodeID) || (tokenInstance != "" && tokenInstance != instanceID) {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) QueuePreparedInstalls(ctx context.Context, instanceID string, request preparedInstallQueueRequest, principal Principal, key, fingerprint string) (domain.Job, bool, error) {
	if len(request.IDs) == 0 || len(request.IDs) > domain.PreparedPackageSlots || key == "" || len(key) > 200 {
		return domain.Job{}, false, errors.New("请选择 1–16 个就绪包并提供幂等键")
	}
	if err := requireRequestFingerprint(key, fingerprint); err != nil {
		return domain.Job{}, false, err
	}
	seen := map[string]bool{}
	for _, id := range request.IDs {
		if uuid.Validate(id) != nil || seen[id] {
			return domain.Job{}, false, errors.New("预下载包 ID 无效或重复")
		}
		seen[id] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback()
	if err := authorizePreparedInstallQueueTx(ctx, tx, principal.User.ID, principal.APITokenID, instanceID); err != nil {
		return domain.Job{}, false, err
	}
	existing, stored, err := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, principal.User.ID, key))
	if err == nil {
		if !requestFingerprintsEqual(stored, fingerprint) {
			return domain.Job{}, false, ErrConflict
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, err
	}
	var nodeID, status string
	if err := tx.QueryRowContext(ctx, `SELECT node_id,status FROM instances WHERE id=?1`, instanceID).Scan(&nodeID, &status); err != nil {
		return domain.Job{}, false, err
	}
	if status == string(domain.InstanceExternal) {
		return domain.Job{}, false, fmt.Errorf("外部实例须先受控重启: %w", ErrConflict)
	}
	for _, id := range request.IDs {
		if _, err := preparedConsumptionPayload(ctx, tx, instanceID, nodeID, "install_mod", map[string]any{"prepared_package_id": id, "acknowledge_native_risk": request.Risk}); err != nil {
			return domain.Job{}, false, err
		}
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, Type: preparedInstallQueueType, Status: domain.JobQueued, RequestedBy: principal.User.ID, IdempotencyKey: key, CreatedAt: time.Now().UTC(), Payload: map[string]any{"prepared_ids": request.IDs, "acknowledge_native_risk": request.Risk}}
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,requested_by,idempotency_key,request_fingerprint,created_at) VALUES(?1,?2,?3,?4,'queued',?5,?6,?7,?8,?9)`, job.ID, nodeID, instanceID, job.Type, string(raw), principal.User.ID, key, fingerprint, job.CreatedAt); err != nil {
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO prepared_install_queues(job_id,instance_id,api_token_id) VALUES(?1,?2,?3)`, job.ID, instanceID, principal.APITokenID); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, fmt.Errorf("本实例已有安装队列，请等待完成或停止后续安装: %w", ErrConflict)
		}
		return domain.Job{}, false, err
	}
	for position, id := range request.IDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO prepared_install_queue_items(queue_id,position,prepared_id) VALUES(?1,?2,?3)`, job.ID, position, id); err != nil {
			return domain.Job{}, false, err
		}
	}
	return job, false, tx.Commit()
}

func finishPreparedInstallQueueTx(ctx context.Context, tx *sql.Tx, id string, status domain.JobStatus, message string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE prepared_install_queue_items SET state='skipped' WHERE queue_id=?1 AND state='waiting'`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prepared_install_queues SET active=0 WHERE job_id=?1`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?2,message=?3,progress=100,finished_at=?4 WHERE id=?1`, id, status, message, time.Now().UTC())
	return err
}

// A bounded step creates at most one child, or records the queue's final state.
// Child creation and the reservation transfer are atomic, including on restart.
func (s *Store) advancePreparedInstallQueue(ctx context.Context, id string, compatible bool, bundleCompatible ...bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active bool
	var tokenID string
	if err := tx.QueryRowContext(ctx, `SELECT active,api_token_id FROM prepared_install_queues WHERE job_id=?1`, id).Scan(&active, &tokenID); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, id))
	if err != nil {
		return false, err
	}
	finish := func(status domain.JobStatus, message string) (bool, error) {
		if err := finishPreparedInstallQueueTx(ctx, tx, id, status, message); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	// Previous child includes the cancellation and terminal-projection barriers.
	var previousStatus domain.JobStatus
	var previousID string
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT j.id,j.status,j.holds_instance_write_lock FROM prepared_install_queue_items q JOIN jobs j ON j.id=q.install_job_id WHERE q.queue_id=?1 ORDER BY q.position DESC LIMIT 1`, id).Scan(&previousID, &previousStatus, &busy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if previousStatus == domain.JobQueued {
			// A disconnect between child creation and dispatch must not leave an
			// expired or revoked intent holding the barrier until reconnection.
			child, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, previousID))
			if err != nil {
				return false, err
			}
			if valid, err := validateQueuedPreparedInstallTx(ctx, tx, child); err != nil {
				return false, err
			} else if !valid {
				return finish(domain.JobFailed, "派发前权限、准备有效期或实例状态已变化，已停止后续安装")
			}
		}
		if busy {
			return false, nil
		}
		if previousStatus != domain.JobSucceeded {
			return finish(domain.JobFailed, "安装失败或已取消，已停止后续安装；请查看子任务")
		}
	}
	var preparedID string
	var position int
	err = tx.QueryRowContext(ctx, `SELECT prepared_id,position FROM prepared_install_queue_items WHERE queue_id=?1 AND state='waiting' ORDER BY position LIMIT 1`, id).Scan(&preparedID, &position)
	if errors.Is(err, sql.ErrNoRows) {
		return finish(domain.JobSucceeded, "所选预下载包已安装完成")
	}
	if err != nil {
		return false, err
	}
	if err := authorizePreparedInstallQueueTx(ctx, tx, job.RequestedBy, tokenID, job.InstanceID); errors.Is(err, ErrUnauthorized) {
		return finish(domain.JobFailed, "安装权限已失效，已停止后续安装")
	} else if err != nil {
		return false, err
	}
	payload, err := preparedConsumptionPayloadForQueue(ctx, tx, job.InstanceID, job.NodeID, "install_mod", map[string]any{"prepared_package_id": preparedID, "acknowledge_native_risk": job.Payload["acknowledge_native_risk"]}, id, "")
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return finish(domain.JobFailed, "预下载包已过期、不可用或目标归属已变化，已停止后续安装")
		}
		return false, err
	}
	var status, installationID string
	if err := tx.QueryRowContext(ctx, `SELECT status,COALESCE(installation_id,'') FROM instances WHERE id=?1 AND node_id=?2`, job.InstanceID, job.NodeID).Scan(&status, &installationID); err != nil {
		return false, err
	}
	if status == string(domain.InstanceExternal) {
		return finish(domain.JobFailed, "实例已变为外部进程，已停止后续安装")
	}
	if !compatible || (stringFromAny(payload["kind"]) == domain.PreparedVPKBundleKind && (len(bundleCompatible) == 0 || !bundleCompatible[0])) {
		return false, nil
	}
	if err := instanceWriteConflict(ctx, tx, job.InstanceID); err != nil {
		var busy *instanceBusyError
		if errors.As(err, &busy) {
			return false, nil
		}
		return false, err
	}
	preparedIDs, ok := job.Payload["prepared_ids"].([]any)
	if !ok || len(preparedIDs) == 0 || len(preparedIDs) > domain.PreparedPackageSlots {
		return finish(domain.JobFailed, "安装队列内容无效")
	}
	childID := uuid.NewString()
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,game_installation_id,type,status,payload,requested_by,created_at) VALUES(?1,?2,?3,NULLIF(?4,''),'install_mod','queued',?5,?6,?7)`, childID, job.NodeID, job.InstanceID, installationID, string(raw), job.RequestedBy, time.Now().UTC()); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prepared_install_queue_items SET state='submitted',install_job_id=?3 WHERE queue_id=?1 AND position=?2`, id, position, childID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running',started_at=COALESCE(started_at,?2),progress=?3,message='按顺序安装中' WHERE id=?1`, id, time.Now().UTC(), position*100/len(preparedIDs)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Once dispatched, a child belongs to the Agent journal. Reconnect must replay
// that exact work; only a never-dispatched child can be rejected locally.
func validateQueuedPreparedInstallTx(ctx context.Context, tx *sql.Tx, job domain.Job) (bool, error) {
	if job.Type != "install_mod" || stringFromAny(job.Payload["prepared_package_id"]) == "" {
		return true, nil
	}
	var queueID, tokenID string
	err := tx.QueryRowContext(ctx, `SELECT q.job_id,q.api_token_id FROM prepared_install_queues q JOIN prepared_install_queue_items i ON i.queue_id=q.job_id WHERE i.install_job_id=?1`, job.ID).Scan(&queueID, &tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	validation := authorizePreparedInstallQueueTx(ctx, tx, job.RequestedBy, tokenID, job.InstanceID)
	if validation == nil {
		_, validation = preparedConsumptionPayloadForQueue(ctx, tx, job.InstanceID, job.NodeID, "install_mod", job.Payload, queueID, job.ID)
	}
	if validation == nil {
		var external bool
		validation = tx.QueryRowContext(ctx, `SELECT status='external' FROM instances WHERE id=?1`, job.InstanceID).Scan(&external)
		if validation == nil && external {
			validation = ErrConflict
		}
	}
	if validation == nil {
		return true, nil
	}
	if !errors.Is(validation, ErrUnauthorized) && !errors.Is(validation, ErrConflict) && !errors.Is(validation, ErrNotFound) {
		return false, validation
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status='failed',progress=100,message='派发前校验未通过：权限、准备有效期或实例状态已变化',finished_at=?2 WHERE id=?1`, job.ID, time.Now().UTC())
	return false, err
}

func (h *AgentHub) advancePreparedInstallQueues(ctx context.Context, nodeID string) {
	// Keyset rotation keeps a busy/offline first page from starving later queues.
	h.mu.RLock()
	cursor := h.preparedQueueCursors[nodeID]
	h.mu.RUnlock()
	rows, err := h.store.db.QueryContext(ctx, `SELECT q.job_id,j.node_id FROM prepared_install_queues q JOIN jobs j ON j.id=q.job_id WHERE q.active=1 AND (?1='' OR j.node_id=?1) AND q.job_id>?2 ORDER BY q.job_id LIMIT 100`, nodeID, cursor)
	if err != nil {
		h.log.Error("load prepared install queues", "error", err)
		return
	}
	type target struct{ id, node string }
	var targets []target
	for rows.Next() {
		var item target
		if err = rows.Scan(&item.id, &item.node); err != nil {
			break
		}
		targets = append(targets, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		h.log.Error("read prepared install queues", "error", err)
		return
	}
	h.mu.Lock()
	if h.preparedQueueCursors == nil {
		h.preparedQueueCursors = make(map[string]string)
	}
	if len(targets) == 100 {
		h.preparedQueueCursors[nodeID] = targets[len(targets)-1].id
	} else {
		delete(h.preparedQueueCursors, nodeID)
	}
	h.mu.Unlock()
	for _, item := range targets {
		changed, err := h.store.advancePreparedInstallQueue(ctx, item.id, h.Supports(item.node, protocol.CapabilityPreparedPackagesV1), h.Supports(item.node, protocol.CapabilityPreparedVPKBundlesV1))
		if err != nil {
			h.log.Error("advance prepared install queue", "job_id", item.id, "error", err)
			continue
		}
		if changed {
			if job, err := h.store.GetJob(ctx, item.id); err == nil {
				_, _ = h.events.Publish(ctx, Event{Type: "job.updated", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
			}
		}
	}
}

func (s *Server) handleQueuePreparedInstalls(w http.ResponseWriter, r *http.Request) {
	var request preparedInstallQueueRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "prepared-install-queue", instanceID, preparedInstallQueueType, map[string]any{"prepared_ids": request.IDs, "acknowledge_native_risk": request.Risk})
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueuePreparedInstalls(r.Context(), instanceID, request, principalFromContext(r.Context()), r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_install_queue", err.Error())
		}
		return
	}
	s.preparedJobAccepted(w, r, job, reused)
}

func (s *Server) handleStopPreparedInstallQueue(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.Type != preparedInstallQueueType || job.InstanceID != chi.URLParam(r, "instanceID") {
		s.fail(w, ErrNotFound)
		return
	}
	s.handleCancelJob(w, r)
}
