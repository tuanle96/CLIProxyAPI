package configaccess

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestProviderRejectsDisabledAPIKey(t *testing.T) {
	t.Parallel()

	key := "sk-disabled"
	p := newProvider("test", []string{key}, map[string]config.APIKeyMetadata{
		config.APIKeyID(key): {Disabled: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	_, authErr := p.Authenticate(req.Context(), req)
	if authErr == nil {
		t.Fatal("expected disabled key to be rejected")
	}
	if got, want := authErr.HTTPStatusCode(), http.StatusForbidden; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestProviderRejectsExpiredAPIKey(t *testing.T) {
	t.Parallel()

	key := "sk-expired"
	p := newProvider("test", []string{key}, map[string]config.APIKeyMetadata{
		config.APIKeyID(key): {ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	_, authErr := p.Authenticate(req.Context(), req)
	if authErr == nil {
		t.Fatal("expected expired key to be rejected")
	}
	if got, want := authErr.HTTPStatusCode(), http.StatusForbidden; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestProviderIncludesAPIKeyMetadata(t *testing.T) {
	t.Parallel()

	key := "sk-active"
	p := newProvider("test", []string{key}, map[string]config.APIKeyMetadata{
		config.APIKeyID(key): {
			Name:        "CI runner",
			Owner:       "platform",
			Environment: "prod",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", key)

	result, authErr := p.Authenticate(req.Context(), req)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if result.Metadata["api_key_id"] != config.APIKeyID(key) {
		t.Fatalf("api_key_id metadata = %q", result.Metadata["api_key_id"])
	}
	if result.Metadata["owner"] != "platform" || result.Metadata["environment"] != "prod" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestProviderEnforcesIPAllowlist(t *testing.T) {
	t.Parallel()

	key := "sk-ip-locked"
	p := newProvider("test", []string{key}, map[string]config.APIKeyMetadata{
		config.APIKeyID(key): {IPAllowlist: []string{"10.0.0.0/8", "127.0.0.1"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "203.0.113.7:443"
	req.Header.Set("Authorization", "Bearer "+key)

	_, authErr := p.Authenticate(req.Context(), req)
	if authErr == nil {
		t.Fatal("expected disallowed remote IP to be rejected")
	}
	if got, want := authErr.HTTPStatusCode(), http.StatusForbidden; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	req.RemoteAddr = "10.2.3.4:443"
	result, authErr := p.Authenticate(req.Context(), req)
	if authErr != nil {
		t.Fatalf("expected allowed CIDR to pass, got %v", authErr)
	}
	if result.Principal != key {
		t.Fatalf("principal = %q, want %q", result.Principal, key)
	}
}
