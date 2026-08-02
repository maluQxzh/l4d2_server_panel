package control

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const bootstrapCookieTTL = 15 * time.Minute

type bootstrapOwnerRequest struct {
	SetupKey string `json:"setup_key"`
}

func (s *Server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	required, err := s.store.NeedsBootstrap(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !required {
		s.clearBootstrapCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"required": false, "key_configured": false})
		return
	}
	keyConfigured, err := s.store.SetupKeyConfigured(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	rawToken := ""
	if cookie, cookieErr := r.Cookie(bootstrapCookieName); cookieErr == nil && validBootstrapCookie(cookie.Value) {
		rawToken = cookie.Value
	}
	if rawToken == "" {
		rawToken, err = randomToken(32)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	expires := time.Now().UTC().Add(bootstrapCookieTTL)
	http.SetCookie(w, &http.Cookie{Name: bootstrapCookieName, Value: rawToken, Path: "/api/v1/auth/bootstrap",
		HttpOnly: true, Secure: s.config.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires,
		MaxAge: int(bootstrapCookieTTL.Seconds())})
	writeJSON(w, http.StatusOK, map[string]any{
		"required":       true,
		"key_configured": keyConfigured,
		"csrf_token":     deriveBootstrapCSRFToken(s.config.MasterKey, rawToken),
	})
}

func (s *Server) handleBootstrapOwner(w http.ResponseWriter, r *http.Request) {
	if !s.validBootstrapCSRF(r) {
		writeError(w, http.StatusForbidden, "bootstrap_csrf_failed", "bootstrap CSRF token is missing or invalid")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	var request bootstrapOwnerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.SetupKey = strings.TrimSpace(request.SetupKey)
	if !ValidSetupKey(request.SetupKey) {
		writeError(w, http.StatusBadRequest, "invalid_setup_key_format", "setup key format is invalid")
		return
	}

	ipAddress := remoteIP(r)
	const limiterIdentity = "__first_owner_bootstrap__"
	if !s.limiter.allow(ipAddress, limiterIdentity) {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many bootstrap attempts")
		return
	}
	// Count every syntactically valid attempt before looking up the key. A
	// successful bootstrap clears the per-operation bucket below.
	s.limiter.failure(ipAddress, limiterIdentity)
	required, err := s.store.NeedsBootstrap(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !required {
		s.clearBootstrapCookie(w)
		writeError(w, http.StatusConflict, "bootstrap_complete", "the first owner has already been created")
		return
	}
	if err := s.store.ValidateSetupKey(r.Context(), request.SetupKey); err != nil {
		s.writeSetupKeyError(w, err)
		return
	}
	hash, err := HashPassword(request.SetupKey)
	if err != nil {
		s.fail(w, err)
		return
	}
	user, err := s.store.BootstrapOwnerWithSetupKey(r.Context(), request.SetupKey, hash)
	if err != nil {
		s.writeSetupKeyError(w, err)
		return
	}
	if bindings, bindingErr := s.store.RoleBindings(r.Context(), user.ID); bindingErr == nil {
		user.RoleBindings = bindings
	}

	sessionToken, err := randomToken(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	csrfToken := deriveCSRFToken(s.config.MasterKey, sessionToken)
	expires := time.Now().UTC().Add(s.config.SessionTTL)
	if _, err := s.store.CreateSession(r.Context(), user.ID, tokenHash(sessionToken), tokenHash(csrfToken), ipAddress,
		r.UserAgent(), expires); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionToken, Path: "/", HttpOnly: true,
		Secure: s.config.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires,
		MaxAge: int(s.config.SessionTTL.Seconds())})
	s.clearBootstrapCookie(w)
	s.limiter.success(ipAddress, limiterIdentity)
	_ = s.store.WriteAudit(r.Context(), domain.AuditEvent{ActorID: user.ID, ActorName: user.Username,
		Action: "auth.bootstrap", ResourceType: "user", ResourceID: user.ID, RemoteIP: ipAddress})
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "csrf_token": csrfToken})
}

func (s *Server) writeSetupKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSetupKeyNotConfigured):
		writeError(w, http.StatusConflict, "setup_key_not_configured", "run the local setup-key command first")
	case errors.Is(err, ErrInvalidSetupKey):
		writeError(w, http.StatusUnauthorized, "invalid_setup_key", "setup key is invalid")
	case errors.Is(err, ErrBootstrapComplete):
		writeError(w, http.StatusConflict, "bootstrap_complete", "the first owner has already been created")
	default:
		s.fail(w, err)
	}
}

func validBootstrapCookie(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 32
}

func (s *Server) validBootstrapCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(bootstrapCookieName)
	if err != nil || !validBootstrapCookie(cookie.Value) {
		return false
	}
	want := deriveBootstrapCSRFToken(s.config.MasterKey, cookie.Value)
	provided := strings.TrimSpace(r.Header.Get("X-Bootstrap-CSRF-Token"))
	return len(want) == len(provided) && subtle.ConstantTimeCompare([]byte(want), []byte(provided)) == 1
}

func (s *Server) clearBootstrapCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: bootstrapCookieName, Value: "", Path: "/api/v1/auth/bootstrap", HttpOnly: true,
		Secure: s.config.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
