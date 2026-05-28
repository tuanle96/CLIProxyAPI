package helps

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PreserveReasoningContent keeps real reasoning_content attached to assistant
// tool-call messages for OpenAI-compatible providers that require replay.
func PreserveReasoningContent(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	msgs := gjson.GetBytes(body, "messages")
	if !msgs.Exists() || !msgs.IsArray() {
		return body
	}

	out := body
	latestReasoning := ""
	hasSeenReasoning := false

	for i, msg := range msgs.Array() {
		if msg.Get("role").String() != "assistant" {
			continue
		}

		if rc := msg.Get("reasoning_content"); rc.Exists() {
			if strings.TrimSpace(rc.String()) != "" {
				latestReasoning = rc.String()
			}
			hasSeenReasoning = true
			continue
		}

		if !hasSeenReasoning {
			continue
		}

		toolCalls := msg.Get("tool_calls")
		if !toolCalls.Exists() || !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}

		if updated, err := sjson.SetBytes(out, "messages."+strconv.Itoa(i)+".reasoning_content", latestReasoning); err == nil {
			out = updated
		}
	}

	return out
}
