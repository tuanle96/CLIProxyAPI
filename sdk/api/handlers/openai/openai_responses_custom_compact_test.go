package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

// customCompactLLMExecutor captures the chat completions call and returns a
// canned compact summary matching the required handoff sections.
type customCompactLLMExecutor struct {
	provider string
	model    string
	alt      string
	calls    int
	payload  []byte

	// responseText is the canned text the executor returns as the LLM response.
	responseText string
}

func (e *customCompactLLMExecutor) Identifier() string { return e.provider }

func (e *customCompactLLMExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.model = req.Model
	e.alt = opts.Alt
	e.payload = append(e.payload[:0], req.Payload...)

	// Build a /chat/completions response with the canned text
	text := e.responseText
	if text == "" {
		text = validCustomCompactOutput()
	}
	chatResp := map[string]interface{}{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
	}
	respBytes, _ := json.Marshal(chatResp)
	return coreexecutor.Response{Payload: respBytes}, nil
}

func (e *customCompactLLMExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *customCompactLLMExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *customCompactLLMExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *customCompactLLMExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// validCustomCompactOutput returns a text that satisfies all required handoff sections.
func validCustomCompactOutput() string {
	return `Current task: Implementing custom compact feature for CLIProxy
User intent: Add LLM-based context compaction when Codex compact fallback is disabled
Repo / location: /Users/test/Projects/CLIProxy
Current state: Configuration and handler changes in progress
Important files:
- internal/config/sdk_config.go: CustomCompactConfig struct added
- sdk/api/handlers/openai/openai_responses_custom_compact.go: Custom compact handler logic
Changes already made:
- Added CustomCompactConfig struct with model, max-tokens, temperature, max-retries fields
- Implemented extractConversationForCompact() to parse compact input
Known verification: All existing compact tests pass
Unfinished work: Integration tests and documentation update
Next action: Run the full test suite to verify custom compact works end-to-end
Do not do: Do not modify the existing compact-fallback behavior when it is enabled`
}

// TestCustomCompactActivatesWhenFallbackDisabled verifies that when
// compact-fallback is disabled and custom-compact is enabled, the proxy
// routes the compact request through the custom LLM compact path.
func TestCustomCompactActivatesWhenFallbackDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	llmExecutor := &customCompactLLMExecutor{provider: "openai-compatibility"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(llmExecutor)

	auth := &coreauth.Auth{ID: "custom-compact-auth", Provider: llmExecutor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "deepseek-v4-pro"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	temp := 0.2
	cfg := &sdkconfig.SDKConfig{}
	cfg.CompactFallback.Enabled = false // disabled
	cfg.CustomCompact.Enabled = true
	cfg.CustomCompact.Model = "deepseek-v4-pro"
	cfg.CustomCompact.Temperature = &temp
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	body := `{"model":"deepseek-v4-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello world"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi there"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if llmExecutor.calls != 1 {
		t.Fatalf("LLM executor calls = %d, want 1", llmExecutor.calls)
	}
	// Verify the executor received a /chat/completions call (alt = "")
	if llmExecutor.alt != "" {
		t.Fatalf("executor alt = %q, want empty (chat completions)", llmExecutor.alt)
	}
	// Verify the response has the Responses API compact format
	respBody := resp.Body.String()
	if !gjson.Get(respBody, "id").Exists() {
		t.Fatalf("response missing id field")
	}
	if gjson.Get(respBody, "object").String() != "response" {
		t.Fatalf("response object = %q, want %q", gjson.Get(respBody, "object").String(), "response")
	}
	if gjson.Get(respBody, "status").String() != "completed" {
		t.Fatalf("response status = %q, want %q", gjson.Get(respBody, "status").String(), "completed")
	}
	outputText := gjson.Get(respBody, "output.0.content.0.text").String()
	if !strings.Contains(outputText, "Current task:") {
		t.Fatalf("output text missing 'Current task:' section; got: %s", outputText[:min(200, len(outputText))])
	}
}

// TestCustomCompactSkippedWhenFallbackEnabled verifies that when
// compact-fallback is enabled, custom compact is NOT used even if
// custom-compact is also enabled.
func TestCustomCompactSkippedWhenFallbackEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexExecutor := &providerCaptureExecutor{provider: "codex"}
	compatExecutor := &customCompactLLMExecutor{provider: "openai-compatibility"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(compatExecutor)

	compatAuth := &coreauth.Auth{ID: "compat-auth-cc", Provider: compatExecutor.Identifier(), Status: coreauth.StatusActive}
	codexAuth := &coreauth.Auth{ID: "codex-auth-cc", Provider: codexExecutor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), compatAuth); err != nil {
		t.Fatalf("Register compat auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), codexAuth); err != nil {
		t.Fatalf("Register codex auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(compatAuth.ID, compatAuth.Provider, []*registry.ModelInfo{{ID: "deepseek-v4-pro"}})
	registry.GetGlobalRegistry().RegisterClient(codexAuth.ID, codexAuth.Provider, []*registry.ModelInfo{{ID: "gpt-5.5"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(compatAuth.ID)
		registry.GetGlobalRegistry().UnregisterClient(codexAuth.ID)
	})

	cfg := &sdkconfig.SDKConfig{}
	cfg.CompactFallback.Enabled = true // enabled -> takes priority
	cfg.CompactFallback.Model = "gpt-5.5"
	cfg.CompactFallback.AppliesToProviders = []string{"*"}
	cfg.CustomCompact.Enabled = true
	cfg.CustomCompact.Model = "deepseek-v4-pro"
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"deepseek-v4-pro","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	// When compact-fallback is enabled, the request should go through the codex
	// executor (via fallback), NOT the custom compact LLM executor.
	if codexExecutor.calls != 1 {
		t.Fatalf("codex executor calls = %d, want 1 (compact fallback should be used)", codexExecutor.calls)
	}
	if compatExecutor.calls != 0 {
		t.Fatalf("compat executor calls = %d, want 0 (custom compact should NOT fire when fallback is enabled)", compatExecutor.calls)
	}
}

// TestCustomCompactSkippedForCodexNativeModel verifies that even when custom
// compact is enabled, models served by a Codex provider use the native compact
// path (no custom compact, no fallback rewrite).
func TestCustomCompactSkippedForCodexNativeModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexExecutor := &providerCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexExecutor)

	codexAuth := &coreauth.Auth{ID: "codex-native-cc", Provider: codexExecutor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), codexAuth); err != nil {
		t.Fatalf("Register codex auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(codexAuth.ID, codexAuth.Provider, []*registry.ModelInfo{{ID: "gpt-5-codex"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(codexAuth.ID)
	})

	cfg := &sdkconfig.SDKConfig{}
	cfg.CompactFallback.Enabled = false
	cfg.CustomCompact.Enabled = true
	cfg.CustomCompact.Model = "gpt-5-codex"
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5-codex","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	// Codex-native model should go directly through the codex executor,
	// NOT through custom compact.
	if codexExecutor.calls != 1 {
		t.Fatalf("codex executor calls = %d, want 1 (native compact should be used)", codexExecutor.calls)
	}
	if codexExecutor.alt != "responses/compact" {
		t.Fatalf("codex alt = %q, want %q", codexExecutor.alt, "responses/compact")
	}
}

// TestCustomCompactSendsCorrectChatPayload verifies that the custom compact
// path builds a proper /chat/completions request with the system prompt and
// extracted conversation content.
func TestCustomCompactSendsCorrectChatPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	llmExecutor := &customCompactLLMExecutor{provider: "openai-compatibility"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(llmExecutor)

	auth := &coreauth.Auth{ID: "payload-check-auth", Provider: llmExecutor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-compact-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	temp := 0.3
	cfg := &sdkconfig.SDKConfig{}
	cfg.CompactFallback.Enabled = false
	cfg.CustomCompact.Enabled = true
	cfg.CustomCompact.Model = "test-compact-model"
	cfg.CustomCompact.MaxTokens = 2048
	cfg.CustomCompact.Temperature = &temp
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	body := `{"model":"test-compact-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the login bug"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will check auth.go"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	// Check the payload sent to the LLM
	payload := string(llmExecutor.payload)

	// Verify it's a chat completions request with system and user messages
	if !strings.Contains(payload, `"role":"system"`) {
		t.Fatalf("payload missing system message")
	}
	if !strings.Contains(payload, `"role":"user"`) {
		t.Fatalf("payload missing user message")
	}
	// Verify the system prompt contains the required sections instruction
	if !strings.Contains(payload, "context compaction assistant") {
		t.Fatalf("payload missing system prompt content")
	}
	// Verify conversation content is in the user message
	if !strings.Contains(payload, "Fix the login bug") {
		t.Fatalf("payload missing user conversation content")
	}
	// Verify stream is false
	if !strings.Contains(payload, `"stream":false`) {
		t.Fatalf("payload should have stream=false")
	}
	// Verify model
	if llmExecutor.model != "test-compact-model" {
		t.Fatalf("executor model = %q, want %q", llmExecutor.model, "test-compact-model")
	}
	// Verify max_tokens
	if !strings.Contains(payload, `"max_tokens":2048`) {
		t.Fatalf("payload max_tokens mismatch; payload=%s", payload)
	}
}

// TestCustomCompactDisabledByDefault verifies that when neither compact-fallback
// nor custom-compact is configured, compact requests go through to the original
// provider's executor unchanged.
func TestCustomCompactDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "default-compact-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	cfg := &sdkconfig.SDKConfig{} // both compact-fallback and custom-compact default to disabled
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	// Should go directly to the original executor with responses/compact alt
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "responses/compact" {
		t.Fatalf("executor alt = %q, want %q", executor.alt, "responses/compact")
	}
}

// TestExtractConversationForCompact verifies the conversation extraction from
// various compact input formats.
func TestExtractConversationForCompact(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "simple string input",
			input:    `{"model":"m","input":"hello world"}`,
			contains: []string{"User: hello world"},
		},
		{
			name: "message array with reasoning",
			input: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"fix bug"}]},
				{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"OPAQUE"},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
			]}`,
			contains: []string{"User: fix bug", "Assistant: done"},
			excludes: []string{"thinking", "OPAQUE"},
		},
		{
			name: "function call and output",
			input: `{"model":"m","input":[
				{"type":"function_call","name":"read_file","arguments":"{\"path\":\"main.go\"}"},
				{"type":"function_call_output","output":"package main"}
			]}`,
			contains: []string{"Function call: read_file", "Function output: package main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractConversationForCompact([]byte(tt.input))
			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("result missing %q; got: %s", s, result)
				}
			}
			for _, s := range tt.excludes {
				if strings.Contains(result, s) {
					t.Errorf("result should not contain %q; got: %s", s, result)
				}
			}
		})
	}
}

// TestValidateCustomCompactOutput verifies output validation logic.
func TestValidateCustomCompactOutput(t *testing.T) {
	t.Run("valid output", func(t *testing.T) {
		ok, missing := validateCustomCompactOutput(validCustomCompactOutput())
		if !ok {
			t.Fatalf("expected valid output, missing: %v", missing)
		}
	})

	t.Run("missing sections", func(t *testing.T) {
		ok, missing := validateCustomCompactOutput("Current task: something\nUser intent: something\n" + strings.Repeat("x", 300))
		if ok {
			t.Fatalf("expected invalid output")
		}
		if len(missing) == 0 {
			t.Fatalf("expected missing sections")
		}
	})

	t.Run("too short", func(t *testing.T) {
		ok, _ := validateCustomCompactOutput("short")
		if ok {
			t.Fatalf("expected invalid output for short text")
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
