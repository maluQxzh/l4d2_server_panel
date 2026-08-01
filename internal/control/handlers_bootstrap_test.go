package control

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateBootstrapOwnerRequest(t *testing.T) {
	valid := bootstrapOwnerRequest{Username: "admin", DisplayName: "系统管理员",
		Password: "correct-horse-battery-staple", PasswordConfirmation: "correct-horse-battery-staple"}
	if err := validateBootstrapOwnerRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	tests := []bootstrapOwnerRequest{
		{Username: "", DisplayName: "Owner", Password: valid.Password, PasswordConfirmation: valid.Password},
		{Username: "admin\nowner", DisplayName: "Owner", Password: valid.Password, PasswordConfirmation: valid.Password},
		{Username: "admin", DisplayName: "", Password: valid.Password, PasswordConfirmation: valid.Password},
		{Username: "admin", DisplayName: "Owner", Password: "too-short", PasswordConfirmation: "too-short"},
		{Username: "admin", DisplayName: "Owner", Password: valid.Password, PasswordConfirmation: "different-password"},
	}
	for index, request := range tests {
		if err := validateBootstrapOwnerRequest(request); err == nil {
			t.Fatalf("invalid request %d was accepted", index)
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
