package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestCustomToolRequestConversion verifies Codex custom tools (e.g. apply_patch,
// {"type":"custom"}) are converted into usable function tools rather than being
// dropped, and that built-in tools (tool_search, image_generation) are removed.
func TestCustomToolRequestConversion(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"edit the file"}]}],
		"tools":[
			{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
			{"type":"custom","name":"apply_patch","description":"Use the apply_patch tool to edit files.","format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}},
			{"type":"tool_search"},
			{"type":"image_generation"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("claude-sonnet-4-5", raw, true)

	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		t.Fatalf("expected tools array, got: %s", out)
	}
	if n := len(tools.Array()); n != 2 {
		t.Fatalf("expected 2 function tools (exec_command + apply_patch), got %d: %s", n, tools.Raw)
	}

	var sawApplyPatch bool
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("type").String() != "function" {
			t.Errorf("all converted tools must be type=function, got %q", tool.Get("type").String())
		}
		if tool.Get("function.name").String() == "apply_patch" {
			sawApplyPatch = true
			// apply_patch must take a free-form string input and keep the grammar hint.
			if tool.Get("function.parameters.properties.input.type").String() != "string" {
				t.Errorf("apply_patch must accept a string input, got: %s", tool.Get("function.parameters").Raw)
			}
			if !strings.Contains(tool.Get("function.description").String(), "grammar") {
				t.Errorf("apply_patch description should retain the grammar hint, got: %s", tool.Get("function.description").String())
			}
		}
		return true
	})
	if !sawApplyPatch {
		t.Fatalf("apply_patch custom tool was dropped instead of converted: %s", tools.Raw)
	}
}

// TestCustomToolCallRoundTrip drives a model tool call to a custom tool through
// the response converter and asserts Codex receives custom_tool_call events with
// the raw (unwrapped) input rather than function_call/JSON arguments.
func TestCustomToolCallRoundTrip(t *testing.T) {
	originalReq := []byte(`{
		"model":"claude-sonnet-4-5",
		"input":[],
		"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: PATCH"}}]
	}`)

	// The model (via Kiro->ChatCompletions) emits a function call to apply_patch
	// whose arguments wrap the raw patch in {"input": ...}.
	chunks := []string{
		`{"id":"resp_1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_patch1","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"resp_1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"*** Begin Patch\\n+hi\\n*** End Patch\"}"}}]},"finish_reason":null}]}`,
		`{"id":"resp_1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
	}

	var param any
	var all []string
	for _, c := range chunks {
		out := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "claude-sonnet-4-5", originalReq, originalReq, []byte(c), &param)
		for _, b := range out {
			all = append(all, string(b))
		}
	}
	// terminal [DONE] to flush response.completed
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "claude-sonnet-4-5", originalReq, originalReq, []byte("[DONE]"), &param)
	for _, b := range out {
		all = append(all, string(b))
	}

	joined := strings.Join(all, "\n")
	t.Logf("emitted:\n%s", joined)

	mustContain := []string{
		`"type":"custom_tool_call"`,
		`"name":"apply_patch"`,
		`"call_id":"call_patch1"`,
		`"type":"response.custom_tool_call_input.done"`,
		`*** Begin Patch`,
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Errorf("custom-tool response missing %q", want)
		}
	}
	// The raw input must NOT be double-wrapped: Codex expects the bare patch text,
	// not {"input":"..."}.
	if strings.Contains(joined, `\"input\":\"*** Begin Patch`) {
		t.Errorf("custom tool input was not unwrapped (still JSON-wrapped):\n%s", joined)
	}
	// Must not emit a function_call item for a custom tool.
	if strings.Contains(joined, `"type":"function_call"`) {
		t.Errorf("custom tool incorrectly emitted as function_call:\n%s", joined)
	}
}
