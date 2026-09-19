package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstanceBusyResponseIdentifiesLockWithoutTaskSecrets(t *testing.T) {
	for _, state := range []struct{ status, result, message string }{
		{"queued", "{}", "排队中"},
		{"dispatched", "{}", "已派发"},
		{"running", "{}", "执行中"},
		{"canceled", `{"cancel_pending":true}`, "等待节点确认取消"},
		{"succeeded", `{"_projection_pending":true}`, "结果尚未同步"},
		{"failed", `{"_projection_pending":true}`, "结果尚未同步"},
	} {
		t.Run(state.status, func(t *testing.T) {
			store, instance := projectionRecoveryStore(t)
			ctx := context.Background()
			if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,result,payload,message)
				VALUES('blocking-job','node',?1,'install_mod',?2,?3,'{"source":"secret-source"}','secret-message')`, instance.ID, state.status, state.result); err != nil {
				t.Fatal(err)
			}
			_, _, queueErr := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", "")
			_, _, _, configErr := store.UpdateInstanceAndQueue(ctx, instance.ID, "renamed", nil, nil, 0, "", "", nil, nil)
			for _, err := range []error{queueErr, configErr} {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("expected conflict, got %v", err)
				}
				recorder := httptest.NewRecorder()
				(&Server{}).fail(recorder, err)
				body := recorder.Body.String()
				if recorder.Code != http.StatusConflict || !strings.Contains(body, `"code":"conflict"`) ||
					!strings.Contains(body, "blocking-job") || !strings.Contains(body, state.message) || strings.Contains(body, "secret-") {
					t.Fatalf("incorrect or leaking busy response: %d %s", recorder.Code, body)
				}
				var structured map[string]map[string]string
				if err := json.Unmarshal(recorder.Body.Bytes(), &structured); err != nil || structured["error"]["reason"] != "instance_busy" || structured["error"]["blocking_job_id"] != "blocking-job" {
					t.Fatalf("missing retry classification: %s (%v)", body, err)
				}
			}
		})
	}
}

func TestGenericConflictDoesNotAllowAutomaticRetry(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{}).fail(recorder, ErrConflict)
	if recorder.Code != http.StatusConflict || strings.Contains(recorder.Body.String(), "instance_busy") {
		t.Fatalf("generic conflict became retryable: %s", recorder.Body.String())
	}
}

func TestInstanceBusyPreservesIdempotentReplay(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	first, _, err := store.QueueInstanceJob(ctx, instance.ID, "stop", nil, "", "same-request")
	if err != nil {
		t.Fatal(err)
	}
	replay, reused, err := store.QueueInstanceJob(ctx, instance.ID, "stop", nil, "", "same-request")
	if err != nil || !reused || replay.ID != first.ID {
		t.Fatalf("write lock broke idempotent replay: %#v, %v, %v", replay, reused, err)
	}
	_, _, err = store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", "same-request")
	var busy *instanceBusyError
	if !errors.Is(err, ErrConflict) || errors.As(err, &busy) {
		t.Fatalf("changed request must remain an idempotency conflict: %v", err)
	}
}
