package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func newActivationTestStore(t *testing.T) (*Store, domain.User) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "activation.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("owner-password-123")
	if err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", hash)
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	return store, owner
}

func TestUserActivationCredentialIsSingleUseAndCompletesAtomically(t *testing.T) {
	ctx := context.Background()
	store, owner := newActivationTestStore(t)
	user, original, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{
		Username: "new-operator", DisplayName: "New Operator",
		Bindings: []domain.RoleBinding{{Role: domain.RoleOperator, ScopeType: "global"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.AccountState != domain.AccountPendingActivation || !strings.HasPrefix(original.OneTimePassword, userActivationCredentialPrefix) {
		t.Fatalf("unexpected pending user or credential: user=%#v credential=%#v", user, original)
	}
	var storedHash []byte
	if err := store.db.QueryRowContext(ctx, `SELECT token_hash FROM user_activation_tokens WHERE user_id=?1`, user.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if len(storedHash) != 32 || strings.Contains(string(storedHash), original.OneTimePassword) {
		t.Fatal("one-time password was not stored as a fixed-size digest")
	}

	reissued, err := store.ReissueUserActivation(ctx, owner.ID, user.ID)
	if err != nil || reissued.OneTimePassword == original.OneTimePassword {
		t.Fatalf("reissue failed or reused a credential: %#v err=%v", reissued, err)
	}
	activationToken := "activation-session-secret"
	activationCSRF := "activation-csrf-secret"
	begin := func(password string) (bool, error) {
		return store.BeginUserActivation(ctx, BeginUserActivationInput{
			UserID: user.ID, OneTimePassword: password, SessionTokenHash: tokenHash(activationToken),
			CSRFTokenHash: tokenHash(activationCSRF), RemoteIP: "127.0.0.1", UserAgent: "test",
			SessionExpiresAt: time.Now().UTC().Add(activationSessionTTL),
		})
	}
	if valid, err := begin(original.OneTimePassword); err != nil || valid {
		t.Fatalf("reissued credential left the old value usable: valid=%v err=%v", valid, err)
	}
	if valid, err := begin(reissued.OneTimePassword); err != nil || !valid {
		t.Fatalf("new credential did not begin activation: valid=%v err=%v", valid, err)
	}
	if valid, err := begin(reissued.OneTimePassword); err != nil || valid {
		t.Fatalf("one-time password was reusable: valid=%v err=%v", valid, err)
	}
	pendingPrincipal, err := store.Session(ctx, tokenHash(activationToken))
	if err != nil || pendingPrincipal.SessionKind != activationSessionKind || pendingPrincipal.User.AccountState != domain.AccountPendingActivation {
		t.Fatalf("activation session was not restricted: %#v err=%v", pendingPrincipal, err)
	}

	newPassword := "new-user-password-456"
	newHash, err := HashPassword(newPassword)
	if err != nil {
		t.Fatal(err)
	}
	standardToken := "standard-session-secret"
	standardCSRF := "standard-csrf-secret"
	if err := store.CompleteUserActivation(ctx, CompleteUserActivationInput{
		UserID: user.ID, ActivationSession: pendingPrincipal.SessionID, PasswordHash: newHash,
		SessionTokenHash: tokenHash(standardToken), CSRFTokenHash: tokenHash(standardCSRF),
		RemoteIP: "127.0.0.1", UserAgent: "test", SessionExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(ctx, tokenHash(activationToken)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("activation session survived password change: %v", err)
	}
	activePrincipal, err := store.Session(ctx, tokenHash(standardToken))
	if err != nil || activePrincipal.SessionKind != standardSessionKind || activePrincipal.User.AccountState != domain.AccountActive {
		t.Fatalf("normal session was not issued atomically: %#v err=%v", activePrincipal, err)
	}
	record, err := store.UserForLogin(ctx, user.Username)
	if err != nil || !VerifyPassword(record.PasswordHash, newPassword) {
		t.Fatalf("new password was not installed: err=%v", err)
	}
	if _, err := store.ReissueUserActivation(ctx, owner.ID, user.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("active account received another activation password: %v", err)
	}
}

func TestExpiredActivationCredentialAndRestrictedSessionFailClosed(t *testing.T) {
	ctx := context.Background()
	store, owner := newActivationTestStore(t)
	user, credential, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{
		Username: "pending-viewer", Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE user_activation_tokens SET expires_at=?2 WHERE user_id=?1`, user.ID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	valid, err := store.BeginUserActivation(ctx, BeginUserActivationInput{
		UserID: user.ID, OneTimePassword: credential.OneTimePassword, SessionTokenHash: tokenHash("session"),
		CSRFTokenHash: tokenHash("csrf"), SessionExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || valid {
		t.Fatalf("expired credential was accepted: valid=%v err=%v", valid, err)
	}

	server := &Server{}
	pending := Principal{User: domain.User{ID: user.ID, AccountState: domain.AccountPendingActivation}, SessionKind: activationSessionKind}
	blocked := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	blocked = blocked.WithContext(context.WithValue(blocked.Context(), principalContextKey, pending))
	blockedResponse := httptest.NewRecorder()
	server.enforceActivatedAccount(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("pending account reached a business handler")
	})).ServeHTTP(blockedResponse, blocked)
	if blockedResponse.Code != http.StatusForbidden || !strings.Contains(blockedResponse.Body.String(), "account_activation_required") {
		t.Fatalf("business API did not fail closed: code=%d body=%s", blockedResponse.Code, blockedResponse.Body.String())
	}

	allowed := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	allowed = allowed.WithContext(context.WithValue(allowed.Context(), principalContextKey, pending))
	allowedResponse := httptest.NewRecorder()
	server.enforceActivatedAccount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusNoContent {
		t.Fatalf("activation-safe endpoint was blocked: %d", allowedResponse.Code)
	}
}
