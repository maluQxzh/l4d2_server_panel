package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newLegacyScheduleTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "legacy-schedules.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO nodes(id,name,status) VALUES('schedule-node','Schedule node','online');
		INSERT INTO instances(id,node_id,name,status,port,config)
			VALUES('schedule-instance','schedule-node','Schedule instance','stopped',27015,'{}');
		INSERT INTO schedules(id,instance_id,name,cron,action,enabled) VALUES
			('legacy-update','schedule-instance','Legacy update','0 4 * * *','update',0),
			('legacy-validate','schedule-instance','Legacy validate','0 5 * * *','validate',0),
			('current-backup','schedule-instance','Current backup','0 6 * * *','backup',0);
	`); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

func TestLegacyMaintenanceScheduleCannotBeReenabled(t *testing.T) {
	store, ctx := newLegacyScheduleTestStore(t)

	if _, err := store.SetScheduleEnabled(ctx, "schedule-instance", "legacy-update", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy update enable error=%v, want conflict", err)
	}
	var enabled bool
	if err := store.db.QueryRowContext(ctx, `SELECT enabled FROM schedules WHERE id='legacy-update'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("rejected legacy schedule became enabled")
	}
	if _, err := store.SetScheduleEnabled(ctx, "schedule-instance", "legacy-update", false); err != nil {
		t.Fatalf("disabling legacy schedule: %v", err)
	}
	if err := store.DeleteSchedule(ctx, "schedule-instance", "legacy-update"); err != nil {
		t.Fatalf("deleting legacy schedule: %v", err)
	}
	if item, err := store.SetScheduleEnabled(ctx, "schedule-instance", "current-backup", true); err != nil || !item.Enabled {
		t.Fatalf("enabling supported schedule: enabled=%v err=%v", item.Enabled, err)
	}
}

func TestSetScheduleEnabledHandlerRejectsLegacyMaintenanceAction(t *testing.T) {
	store, _ := newLegacyScheduleTestStore(t)
	server := &Server{store: store}
	request := httptest.NewRequest(http.MethodPatch,
		"/api/v1/instances/schedule-instance/schedules/legacy-validate", strings.NewReader(`{"enabled":true}`))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("instanceID", "schedule-instance")
	routeContext.URLParams.Add("scheduleID", "legacy-validate")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	response := httptest.NewRecorder()

	server.handleSetScheduleEnabled(response, request)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no longer supported") {
		t.Fatalf("handler response=%d %s, want conflict", response.Code, response.Body.String())
	}
}
