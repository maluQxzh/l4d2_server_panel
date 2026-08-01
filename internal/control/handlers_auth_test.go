package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterUsesCombinationAndIPAddressBuckets(t *testing.T) {
	limiter := newLoginLimiter()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }

	for range loginComboLimit {
		if !limiter.allow("192.0.2.1", "owner") {
			t.Fatal("combination was limited too early")
		}
		limiter.failure("192.0.2.1", "owner")
	}
	if limiter.allow("192.0.2.1", "owner") {
		t.Fatal("IP/username combination did not reach its limit")
	}
	if !limiter.allow("192.0.2.1", "another-user") {
		t.Fatal("IP-wide limit was reached before its configured threshold")
	}

	for i := loginComboLimit; i < loginIPLimit; i++ {
		username := "rotated-" + string(rune('a'+i-loginComboLimit))
		limiter.failure("192.0.2.1", username)
	}
	if limiter.allow("192.0.2.1", "fresh-username") {
		t.Fatal("rotating usernames bypassed the IP-wide limit")
	}

	now = now.Add(loginAttemptWindow)
	if !limiter.allow("192.0.2.1", "owner") {
		t.Fatal("expired login buckets were not reset")
	}
}

func TestLoginLimiterCapacityIsBounded(t *testing.T) {
	limiter := newLoginLimiter()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	for i := 0; i < loginLimiterMaxKeys+100; i++ {
		limiter.failure("198.51.100.10", "user-"+time.Unix(int64(i), 0).Format(time.RFC3339Nano))
		now = now.Add(time.Millisecond)
	}
	if got := len(limiter.attempts); got > loginLimiterMaxKeys {
		t.Fatalf("limiter retained %d keys, maximum is %d", got, loginLimiterMaxKeys)
	}
}

func TestLoginRejectsOversizedAndMalformedFieldsBeforeStoreLookup(t *testing.T) {
	server := &Server{limiter: newLoginLimiter()}
	tests := []string{
		`{"username":"` + strings.Repeat("u", 129) + `","password":"valid-password"}`,
		`{"username":"owner","password":"` + strings.Repeat("p", 1025) + `"}`,
		`{"username":"owner","password":"valid-password","totp_code":"12x456"}`,
	}
	for _, body := range tests {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		response := httptest.NewRecorder()
		server.handleLogin(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid login request returned %d: %s", response.Code, response.Body.String())
		}
	}
}
