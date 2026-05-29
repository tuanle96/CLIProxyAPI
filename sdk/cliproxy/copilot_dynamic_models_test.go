package cliproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	copilotauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestVerifyCopilotChatCompletionModelsKeepsOnlyCallableModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "unsupported" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The requested model is not supported.","code":"model_not_supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	models := []*registry.ModelInfo{{ID: "callable"}, {ID: "unsupported"}}
	got := verifyCopilotChatCompletionModels(copilotauth.NewCopilotAuth(nil), "auth-1", server.URL, "token", models)
	if len(got) != 1 {
		t.Fatalf("verified models = %d, want 1: %#v", len(got), got)
	}
	if got[0].ID != "callable" {
		t.Fatalf("verified model = %q, want callable", got[0].ID)
	}
}
