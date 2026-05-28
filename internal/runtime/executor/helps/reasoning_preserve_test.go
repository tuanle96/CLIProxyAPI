package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestPreserveReasoningContent_InjectsFromHistory(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"plan","reasoning_content":"real reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_2","type":"function","function":{"name":"write","arguments":"{}"}}]}
	]}`)

	out := PreserveReasoningContent(body)

	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "real reasoning" {
		t.Fatalf("messages.2.reasoning_content = %q, want real reasoning", got)
	}
}

func TestPreserveReasoningContent_KeepsExistingValue(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"a","reasoning_content":"first","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"},
		{"role":"assistant","content":"","reasoning_content":"second","tool_calls":[{"id":"call_2","type":"function","function":{"name":"write","arguments":"{}"}}]}
	]}`)

	out := PreserveReasoningContent(body)

	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "second" {
		t.Fatalf("messages.2.reasoning_content = %q, want second", got)
	}
}

func TestPreserveReasoningContent_DoesNotFabricateWhenAbsent(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]}
	]}`)

	out := PreserveReasoningContent(body)

	if gjson.GetBytes(out, "messages.1.reasoning_content").Exists() {
		t.Fatalf("unexpected fabricated reasoning_content: %s", string(out))
	}
}

func TestPreserveReasoningContent_EmptyDoesNotOverwritePriorNonEmpty(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"","reasoning_content":"keep this","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"assistant","content":"","reasoning_content":""},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_2","type":"function","function":{"name":"write","arguments":"{}"}}]}
	]}`)

	out := PreserveReasoningContent(body)

	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "keep this" {
		t.Fatalf("messages.2.reasoning_content = %q, want keep this", got)
	}
}

func TestPreserveReasoningContent_AllEmptyReasoningInjectsEmpty(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"","reasoning_content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_2","type":"function","function":{"name":"write","arguments":"{}"}}]}
	]}`)

	out := PreserveReasoningContent(body)

	rc := gjson.GetBytes(out, "messages.1.reasoning_content")
	if !rc.Exists() || rc.String() != "" {
		t.Fatalf("messages.1.reasoning_content = %q exists=%t, want empty string", rc.String(), rc.Exists())
	}
}

func TestPreserveReasoningContent_SkipsAssistantWithoutToolCalls(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":"answer","reasoning_content":"real reasoning"},
		{"role":"assistant","content":"plain answer"}
	]}`)

	out := PreserveReasoningContent(body)

	if gjson.GetBytes(out, "messages.1.reasoning_content").Exists() {
		t.Fatalf("unexpected reasoning_content on assistant without tool_calls: %s", string(out))
	}
}
