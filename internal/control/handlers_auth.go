package control

import (
	"encoding/base32"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/pquerna/otp/totp"
)

type loginAttempt struct {
	count int
	since time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
	now      func() time.Time
	checks   uint64
}

const (
	loginAttemptWindow  = 5 * time.Minute
	loginComboLimit     = 8
	loginIPLimit        = 32
	loginLimiterMaxKeys = 10_000
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt), now: time.Now}
}

func (l *loginLimiter) allow(ipAddress, username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.periodicCleanupLocked(now)
	ipAttempt := l.activeAttemptLocked("ip\x00"+ipAddress, now)
	comboAttempt := l.activeAttemptLocked("combo\x00"+ipAddress+"\x00"+normalizeUsername(username), now)
	return ipAttempt.count < loginIPLimit && comboAttempt.count < loginComboLimit
}

func (l *loginLimiter) failure(ipAddress, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.periodicCleanupLocked(now)
	l.incrementLocked("ip\x00"+ipAddress, now)
	l.incrementLocked("combo\x00"+ipAddress+"\x00"+normalizeUsername(username), now)
}

func (l *loginLimiter) success(ipAddress, username string) {
	l.mu.Lock()
	// Preserve the IP-wide failure bucket so a single successful credential
	// cannot reset an attack against many usernames from the same address.
	delete(l.attempts, "combo\x00"+ipAddress+"\x00"+normalizeUsername(username))
	l.mu.Unlock()
}

func (l *loginLimiter) activeAttemptLocked(key string, now time.Time) loginAttempt {
	attempt := l.attempts[key]
	if !attempt.since.IsZero() && now.Sub(attempt.since) >= loginAttemptWindow {
		delete(l.attempts, key)
		return loginAttempt{}
	}
	return attempt
}

func (l *loginLimiter) incrementLocked(key string, now time.Time) {
	attempt := l.activeAttemptLocked(key, now)
	if attempt.since.IsZero() {
		attempt = loginAttempt{since: now}
	}
	attempt.count++
	if _, exists := l.attempts[key]; !exists && len(l.attempts) >= loginLimiterMaxKeys {
		l.cleanupLocked(now)
		if len(l.attempts) >= loginLimiterMaxKeys {
			l.evictOldestLocked()
		}
	}
	l.attempts[key] = attempt
}

func (l *loginLimiter) periodicCleanupLocked(now time.Time) {
	l.checks++
	if l.checks%256 == 0 {
		l.cleanupLocked(now)
	}
}

func (l *loginLimiter) cleanupLocked(now time.Time) {
	for key, attempt := range l.attempts {
		if attempt.since.IsZero() || now.Sub(attempt.since) >= loginAttemptWindow {
			delete(l.attempts, key)
		}
	}
}

func (l *loginLimiter) evictOldestLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, attempt := range l.attempts {
		if oldestKey == "" || attempt.since.Before(oldest) {
			oldestKey, oldest = key, attempt.since
		}
	}
	if oldestKey != "" {
		delete(l.attempts, oldestKey)
	}
}

type loginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	TOTPCode     string `json:"totp_code,omitempty"`
	RecoveryCode string `json:"recovery_code,omitempty"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if err := validateLoginRequest(request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_login_request", err.Error())
		return
	}
	ipAddress := remoteIP(r)
	if !s.limiter.allow(ipAddress, request.Username) {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts")
		return
	}
	record, err := s.store.UserForLogin(r.Context(), request.Username)
	if err != nil || record.Disabled || !VerifyPassword(record.PasswordHash, request.Password) {
		s.limiter.failure(ipAddress, request.Username)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "username or password is invalid")
		return
	}
	if record.User.TOTPEnabled {
		totpCode := strings.TrimSpace(request.TOTPCode)
		recoveryCode := normalizeRecoveryCode(request.RecoveryCode)
		if totpCode == "" && recoveryCode == "" {
			s.limiter.failure(ipAddress, request.Username)
			writeError(w, http.StatusUnauthorized, "totp_required", "a TOTP or recovery code is required")
			return
		}
		secondFactorValid := false
		if totpCode != "" {
			secret, decryptErr := decryptSecret(s.config.MasterKey, record.TOTPSecret)
			secondFactorValid = decryptErr == nil && totp.Validate(totpCode, secret)
		} else {
			secondFactorValid, err = s.store.ConsumeTOTPRecoveryCode(r.Context(), record.User.ID, recoveryCodeHash(recoveryCode))
			if err != nil {
				s.fail(w, err)
				return
			}
		}
		if !secondFactorValid {
			s.limiter.failure(ipAddress, request.Username)
			writeError(w, http.StatusUnauthorized, "invalid_totp", "the TOTP or recovery code is invalid")
			return
		}
	}
	if bindings, bindingErr := s.store.RoleBindings(r.Context(), record.User.ID); bindingErr == nil {
		record.User.RoleBindings = bindings
	}
	sessionToken, err := randomToken(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	csrfToken := deriveCSRFToken(s.config.MasterKey, sessionToken)
	expires := time.Now().UTC().Add(s.config.SessionTTL)
	if _, err := s.store.CreateSession(r.Context(), record.User.ID, tokenHash(sessionToken), tokenHash(csrfToken), remoteIP(r), r.UserAgent(), expires); err != nil {
		s.fail(w, err)
		return
	}
	s.limiter.success(ipAddress, request.Username)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionToken, Path: "/", HttpOnly: true,
		Secure: s.config.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(s.config.SessionTTL.Seconds())})
	_ = s.store.WriteAudit(r.Context(), domain.AuditEvent{ActorID: record.User.ID, ActorName: record.User.Username,
		Action: "auth.login", ResourceType: "session", RemoteIP: remoteIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"user": record.User, "csrf_token": csrfToken})
}

func validateLoginRequest(request loginRequest) error {
	if len(request.Username) == 0 || len(request.Username) > 128 {
		return errors.New("username must be between 1 and 128 bytes")
	}
	if len(request.Password) == 0 || len(request.Password) > 1024 {
		return errors.New("password must be between 1 and 1024 bytes")
	}
	code := strings.TrimSpace(request.TOTPCode)
	recovery := normalizeRecoveryCode(request.RecoveryCode)
	if code != "" && recovery != "" {
		return errors.New("provide either a TOTP code or a recovery code, not both")
	}
	if code != "" {
		if len(code) != 6 {
			return errors.New("TOTP code must contain exactly 6 digits")
		}
		for i := 0; i < len(code); i++ {
			if code[i] < '0' || code[i] > '9' {
				return errors.New("TOTP code must contain exactly 6 digits")
			}
		}
	}
	if recovery != "" {
		if len(recovery) != 16 {
			return errors.New("recovery code is invalid")
		}
		for _, character := range recovery {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", character) {
				return errors.New("recovery code is invalid")
			}
		}
	}
	return nil
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.IsAPIToken() {
		writeJSON(w, http.StatusOK, map[string]any{"user": principal.User, "api_token": true, "csrf_token": ""})
		return
	}
	cookie, _ := r.Cookie(sessionCookieName)
	csrf := deriveCSRFToken(s.config.MasterKey, cookie.Value)
	writeJSON(w, http.StatusOK, map[string]any{"user": principal.User, "csrf_token": csrf})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if err := s.store.DeleteSession(r.Context(), principal.SessionID); err != nil {
		s.fail(w, err)
		return
	}
	s.events.InvalidateAuthorization()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: s.config.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	s.audit(r, "auth.logout", "session", principal.SessionID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.User.TOTPEnabled {
		writeError(w, http.StatusConflict, "totp_already_enabled", "TOTP is already enabled")
		return
	}
	issuer := "L4D2 Panel"
	key, err := totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: principal.User.Username})
	if err != nil {
		s.fail(w, err)
		return
	}
	encrypted, err := encryptSecret(s.config.MasterKey, key.Secret())
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.SaveTOTPSecret(r.Context(), principal.User.ID, encrypted); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "auth.totp.setup", "user", principal.User.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"secret": key.Secret(), "otpauth_url": key.URL()})
}

func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	principal := principalFromContext(r.Context())
	encrypted, _, err := s.store.TOTPSecret(r.Context(), principal.User.ID)
	if err != nil || encrypted == "" {
		writeError(w, http.StatusConflict, "totp_not_initialized", "start TOTP setup first")
		return
	}
	secret, err := decryptSecret(s.config.MasterKey, encrypted)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !totp.Validate(strings.TrimSpace(request.Code), secret) {
		writeError(w, http.StatusBadRequest, "invalid_totp", "the TOTP code is invalid")
		return
	}
	recoveryCodes, recoveryDigests, err := newTOTPRecoveryCodes(10)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.EnableTOTPWithRecoveryCodes(r.Context(), principal.User.ID, recoveryDigests); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "auth.totp.enabled", "user", principal.User.ID, nil)
	// Plaintext recovery codes are intentionally returned only by this response.
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "recovery_codes": recoveryCodes})
}

func newTOTPRecoveryCodes(count int) ([]string, [][]byte, error) {
	if count < 1 || count > 100 {
		return nil, nil, errors.New("invalid recovery code count")
	}
	codes := make([]string, 0, count)
	digests := make([][]byte, 0, count)
	seen := make(map[string]struct{}, count)
	for len(codes) < count {
		raw, err := randomToken(10)
		if err != nil {
			return nil, nil, err
		}
		// Hashing the CSPRNG output before base32 encoding keeps the display
		// alphabet unambiguous while retaining 80 bits of entropy per code.
		bytes := tokenHash(raw)[:10]
		normalized := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes)
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		display := normalized[0:4] + "-" + normalized[4:8] + "-" + normalized[8:12] + "-" + normalized[12:16]
		codes = append(codes, display)
		digests = append(digests, recoveryCodeHash(normalized))
	}
	return codes, digests, nil
}

func normalizeRecoveryCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	return strings.NewReplacer("-", "", " ", "").Replace(value)
}

func recoveryCodeHash(normalized string) []byte {
	return tokenHash("l4d2panel:totp-recovery:v1:" + normalized)
}

func totpURL(issuer, account, secret string) string {
	values := url.Values{"secret": {secret}, "issuer": {issuer}}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + values.Encode()
}
