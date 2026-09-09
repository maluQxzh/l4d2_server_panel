package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func teamDeleteFixture(t *testing.T) (*Store, context.Context, domain.User, domain.User) {
	t.Helper()
	store, ctx := openNodeTestStore(t)
	owner, _, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{
		Username: "target", Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, ctx, owner, target
}

func TestDeleteTeamUserRemovesCredentialsAndRetainsHistory(t *testing.T) {
	for _, state := range []string{"pending_activation", "active", "disabled"} {
		t.Run(state, func(t *testing.T) {
			store, ctx, owner, target := teamDeleteFixture(t)
			if state != "pending_activation" {
				if _, err := store.db.ExecContext(ctx, `UPDATE users SET account_state='active',disabled=?2 WHERE id=?1`, target.ID, state == "disabled"); err != nil {
					t.Fatal(err)
				}
			}
			expires := time.Now().Add(time.Hour)
			if _, err := store.CreateSession(ctx, target.ID, tokenHash("session"), tokenHash("csrf"), "", "", expires); err != nil {
				t.Fatal(err)
			}
			token, _, err := store.CreateEnrollmentToken(ctx, "history-node", target.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			node, _, err := store.ConsumeEnrollmentToken(ctx, EnrollRequest{Token: token, Name: "node", OS: "linux", Arch: "amd64", InstanceRoot: "/srv/game"}, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `
				INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask,expires_at) VALUES('token',?1,zeroblob(32),'test',1,?2);
				INSERT INTO totp_recovery_codes(id,user_id,code_hash) VALUES('recovery',?1,zeroblob(32));
				INSERT INTO jobs(id,node_id,type,status,requested_by,idempotency_key) VALUES('user-job',?3,'logs.search','succeeded',?1,'shared-key');
				INSERT INTO jobs(id,node_id,type,status,idempotency_key) VALUES('system-job',?3,'logs.search','succeeded','shared-key');
				INSERT INTO maps(id,node_id,name,aggregate_sha256,total_size,created_by) VALUES('map',?3,'retained map',?4,1,?1);
			`, target.ID, expires, node.ID, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if err := store.WriteAudit(ctx, domain.AuditEvent{ActorID: target.ID, ActorName: "Original actor", Action: "test.history", ResourceType: "node", ResourceID: node.ID}); err != nil {
				t.Fatal(err)
			}
			if deleted, err := store.DeleteTeamUser(ctx, owner.ID, target.ID); err != nil || !deleted {
				t.Fatalf("delete: deleted=%v err=%v", deleted, err)
			}
			for _, table := range []string{"sessions", "api_tokens", "totp_recovery_codes", "role_bindings", "user_activation_tokens"} {
				var count int
				if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE user_id=?1`, target.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s retained credentials: count=%d err=%v", table, count, err)
				}
			}
			var count int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions WHERE binding_id=?1`, target.RoleBindings[0].ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("binding permissions retained: %d, %v", count, err)
			}
			for _, query := range []string{
				`SELECT count(*) FROM jobs WHERE id='user-job' AND requested_by IS NULL AND idempotency_key IS NULL AND status='succeeded'`,
				`SELECT count(*) FROM jobs WHERE id='system-job' AND idempotency_key='shared-key'`,
				`SELECT count(*) FROM maps WHERE id='map' AND created_by IS NULL`,
				`SELECT count(*) FROM nodes`,
				`SELECT count(*) FROM audit_events WHERE action='test.history' AND actor_name='Original actor' AND actor_id IS NULL`,
			} {
				if err := store.db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 1 {
					t.Fatalf("history changed: %s count=%d err=%v", query, count, err)
				}
			}
			if _, err := store.TeamUser(ctx, target.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted account remains: %v", err)
			}
			if _, err := store.Session(ctx, tokenHash("session")); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("deleted session remains usable: %v", err)
			}
			if deleted, err := store.DeleteTeamUser(ctx, owner.ID, target.ID); err != nil || deleted {
				t.Fatalf("repeat delete: deleted=%v err=%v", deleted, err)
			}
			if replacement, _, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{Username: target.Username, Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global"}}}); err != nil || replacement.ID == target.ID {
				t.Fatalf("username cannot be reused with a new identity: %v", err)
			}
		})
	}
}

func TestDeleteTeamUserProtectsOwnersAndRechecksActor(t *testing.T) {
	store, ctx, owner, target := teamDeleteFixture(t)
	if _, err := store.DeleteTeamUser(ctx, target.ID, owner.ID); !errors.Is(err, errRootSecurityRequired) {
		t.Fatalf("pending/non-owner actor accepted: %v", err)
	}
	if _, err := store.DeleteTeamUser(ctx, owner.ID, owner.ID); !errors.Is(err, errProtectedUser) {
		t.Fatalf("self deletion accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE users SET account_state='active' WHERE id=?1`, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteTeamUser(ctx, target.ID, "missing"); !errors.Is(err, errRootSecurityRequired) {
		t.Fatalf("active non-owner accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE users SET account_state='active',role='owner' WHERE id=?1;
		UPDATE role_bindings SET role='owner' WHERE user_id=?1`, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteTeamUser(ctx, target.ID, owner.ID); !errors.Is(err, errProtectedUser) {
		t.Fatalf("bootstrap owner deletion accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE users SET disabled=1 WHERE id=?1`, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteTeamUser(ctx, target.ID, "missing"); !errors.Is(err, errRootSecurityRequired) {
		t.Fatalf("disabled owner accepted even for a missing target: %v", err)
	}
	if deleted, err := store.DeleteTeamUser(ctx, owner.ID, target.ID); err != nil || !deleted {
		t.Fatalf("ordinary owner cannot be deleted: %v", err)
	}
}

func TestDeleteTeamUserFailureRollsBackCredentialRemoval(t *testing.T) {
	store, ctx, owner, target := teamDeleteFixture(t)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_user_delete BEFORE DELETE ON users BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.DeleteTeamUser(ctx, owner.ID, target.ID); err == nil || deleted {
		t.Fatalf("failed delete reported success: %v", err)
	}
	user, err := store.TeamUser(ctx, target.ID)
	if err != nil || len(user.RoleBindings) != len(target.RoleBindings) {
		t.Fatalf("failed delete changed account: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM user_activation_tokens WHERE user_id=?1`, target.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed delete removed activation credential: count=%d err=%v", count, err)
	}
}

func TestDeleteUserHandlerInvalidatesAuthorizationAndAuditsOnce(t *testing.T) {
	store, ctx, owner, target := teamDeleteFixture(t)
	server := &Server{store: store, events: NewEventBroker(store)}
	principal := Principal{User: owner}
	for _, apiToken := range []bool{true, false, false} {
		principal.APITokenID = ""
		if apiToken {
			principal.APITokenID = "owner-token"
		}
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+target.ID, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("userID", target.ID)
		request = request.WithContext(context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, route), principalContextKey, principal))
		request.Header.Set("Idempotency-Key", "same-delete")
		response := httptest.NewRecorder()
		before := server.events.AuthorizationGeneration()
		server.handleDeleteUser(response, request)
		if apiToken {
			if response.Code != http.StatusForbidden {
				t.Fatalf("API token delete returned %d", response.Code)
			}
			continue
		}
		if response.Code != http.StatusNoContent || server.events.AuthorizationGeneration() <= before {
			t.Fatalf("delete status=%d body=%s or authorization not invalidated", response.Code, response.Body.String())
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='user.deleted' AND resource_id=?1`, target.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delete audit count=%d err=%v", count, err)
	}
}
