package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateSetupKeyFormat(t *testing.T) {
	valid := setupKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if !ValidSetupKey(valid) {
		t.Fatal("valid setup key was rejected")
	}
	for _, value := range []string{"", valid + "x", strings.TrimPrefix(valid, setupKeyPrefix), setupKeyPrefix + strings.Repeat("!", 43)} {
		if ValidSetupKey(value) {
			t.Fatalf("invalid setup key was accepted: %q", value)
		}
	}
}

func TestSetupKeyErrorsHaveStablePublicCodes(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrSetupKeyNotConfigured, http.StatusConflict, "setup_key_not_configured"},
		{ErrInvalidSetupKey, http.StatusUnauthorized, "invalid_setup_key"},
		{ErrBootstrapComplete, http.StatusConflict, "bootstrap_complete"},
		{errors.New("database failed"), http.StatusInternalServerError, "internal_error"},
	} {
		response := httptest.NewRecorder()
		server.writeSetupKeyError(response, test.err)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != test.status || body.Error.Code != test.code {
			t.Fatalf("error %v returned status=%d code=%q", test.err, response.Code, body.Error.Code)
		}
	}
}

func TestBootstrapCSRFRequiresMatchingCookieAndHeader(t *testing.T) {
	server := &Server{config: Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}}
	rawToken := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	want := deriveBootstrapCSRFToken(server.config.MasterKey, rawToken)

	valid := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bootstrap", strings.NewReader(`{}`))
	valid.AddCookie(&http.Cookie{Name: bootstrapCookieName, Value: rawToken})
	valid.Header.Set("X-Bootstrap-CSRF-Token", want)
	if !server.validBootstrapCSRF(valid) {
		t.Fatal("matching bootstrap cookie and CSRF header were rejected")
	}

	for _, mutate := range []func(*http.Request){
		func(request *http.Request) { request.Header.Del("X-Bootstrap-CSRF-Token") },
		func(request *http.Request) { request.Header.Set("X-Bootstrap-CSRF-Token", want+"x") },
		func(request *http.Request) { request.Header.Set("Cookie", bootstrapCookieName+"=invalid") },
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/bootstrap", strings.NewReader(`{}`))
		request.AddCookie(&http.Cookie{Name: bootstrapCookieName, Value: rawToken})
		request.Header.Set("X-Bootstrap-CSRF-Token", want)
		mutate(request)
		if server.validBootstrapCSRF(request) {
			t.Fatal("invalid bootstrap CSRF request was accepted")
		}
	}

	if deriveCSRFToken(server.config.MasterKey, rawToken) == want {
		t.Fatal("bootstrap and authenticated-session CSRF domains are not separated")
	}
}
