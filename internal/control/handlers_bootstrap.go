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
	Username             string `json:"username"`
	DisplayName          string `json:"display_name"`
	Password             string `json:"password"`
	PasswordConfirmation string `json:"password_confirmation"`
}

func (s *Server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	required, err := s.store.NeedsBootstrap(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !required {
		s.clearBootstrapCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
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
		"required":   true,
		"csrf_token": deriveBootstrapCSRFToken(s.config.MasterKey, rawToken),
	})
}

func (s *Server) handleBootstrapOwner(w http.ResponseWriter, r *http.Request) {
	if !s.validBootstrapCSRF(r) {
		writeError(w, http.StatusForbidden, "bootstrap_csrf_failed", "bootstrap CSRF token is missing or invalid")
		return
	}
	var request bootstrapOwnerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Username = strings.TrimSpace(request.Username)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if err := validateBootstrapOwnerRequest(request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bootstrap_request", err.Error())
		return
	}

	ipAddress := remoteIP(r)
	const limiterIdentity = "__first_owner_bootstrap__"
	if !s.limiter.allow(ipAddress, limiterIdentity) {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many bootstrap attempts")
		return
	}
	// Count every request that reaches the expensive password hash. A successful
	// bootstrap clears the per-operation bucket below; failures and races stay
	// bounded even when they use otherwise valid input.
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
	hash, err := HashPassword(request.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bootstrap_request", err.Error())
		return
	}
	user, created, err := s.store.BootstrapOwner(r.Context(), request.Username, request.DisplayName, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !created {
		s.clearBootstrapCookie(w)
		writeError(w, http.StatusConflict, "bootstrap_complete", "the first owner has already been created")
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

func validateBootstrapOwnerRequest(request bootstrapOwnerRequest) error {
	if len(request.Username) < 1 || len(request.Username) > 128 || containsControlCharacter(request.Username) {
		return errors.New("username must be between 1 and 128 bytes without control characters")
	}
	if len(request.DisplayName) < 1 || len(request.DisplayName) > 128 || containsControlCharacter(request.DisplayName) {
		return errors.New("display_name must be between 1 and 128 bytes without control characters")
	}
	if request.Password != request.PasswordConfirmation {
		return errors.New("password confirmation does not match")
	}
	if len(request.Password) < 12 || len(request.Password) > 1024 || strings.ContainsAny(request.Password, "\r\n\x00") {
		return errors.New("password must be a single line between 12 and 1024 characters")
	}
	return nil
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
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
