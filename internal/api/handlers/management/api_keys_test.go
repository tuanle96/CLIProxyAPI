package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGetAPIKeysReturnsMetadataItems(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	key := "sk-test-key-123456"
	h := &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				APIKeys: []string{key},
				APIKeyMetadata: map[string]config.APIKeyMetadata{
					config.APIKeyID(key): {
						Name:        "CI runner",
						Owner:       "platform",
						Environment: "prod",
					},
				},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)

	h.GetAPIKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body apiKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.APIKeys) != 1 || body.APIKeys[0] != key {
		t.Fatalf("api-keys = %#v", body.APIKeys)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(body.Items))
	}
	if got := body.Items[0].Metadata.Owner; got != "platform" {
		t.Fatalf("owner = %q, want platform", got)
	}
	if got := body.Items[0].Status; got != config.APIKeyStatusActive {
		t.Fatalf("status = %q, want active", got)
	}
}

func TestPutAPIKeysPersistsMetadataObject(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: writeTestConfigFile(t),
	}

	body := apiKeyWriteBody{
		Items: []apiKeyWriteItem{
			{
				Key: "sk-new-key",
				Metadata: &config.APIKeyMetadata{
					Name:        "Build agent",
					Owner:       "devops",
					Environment: "staging",
					Disabled:    true,
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutAPIKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(h.cfg.APIKeys) != 1 || h.cfg.APIKeys[0] != "sk-new-key" {
		t.Fatalf("api keys = %#v", h.cfg.APIKeys)
	}
	meta := h.cfg.APIKeyMetadata[config.APIKeyID("sk-new-key")]
	if meta.Owner != "devops" || !meta.Disabled {
		t.Fatalf("metadata = %#v", meta)
	}
	if meta.CreatedAt == "" || meta.UpdatedAt == "" || len(meta.Audit) == 0 {
		t.Fatalf("expected lifecycle metadata, got %#v", meta)
	}
}

func TestPutAPIKeysRejectsInvalidPolicyMetadata(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: writeTestConfigFile(t),
	}
	body := apiKeyWriteBody{
		Items: []apiKeyWriteItem{
			{
				Key: "sk-invalid-policy",
				Metadata: &config.APIKeyMetadata{
					ExpiresAt:   "not-a-date",
					QuotaPeriod: "monthly",
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutAPIKeys(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPatchAPIKeysMovesMetadataOnRotation(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	oldKey := "sk-old-key"
	newKey := "sk-new-key"
	h := &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				APIKeys: []string{oldKey},
				APIKeyMetadata: map[string]config.APIKeyMetadata{
					config.APIKeyID(oldKey): {Owner: "platform"},
				},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", bytes.NewReader([]byte(`{"index":0,"value":"`+newKey+`"}`)))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAPIKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if _, exists := h.cfg.APIKeyMetadata[config.APIKeyID(oldKey)]; exists {
		t.Fatal("old key metadata should be removed after rotation")
	}
	meta := h.cfg.APIKeyMetadata[config.APIKeyID(newKey)]
	if meta.Owner != "platform" || meta.LastRotatedAt == "" {
		t.Fatalf("metadata = %#v", meta)
	}
}
