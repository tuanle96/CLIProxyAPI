package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	log "github.com/sirupsen/logrus"
)

// customCompactSystemPrompt is the system prompt sent to the LLM for context
// compaction. It instructs the model to produce a structured execution handoff
// that preserves the critical state needed for the coding agent to resume.
const customCompactSystemPrompt = `You are a context compaction assistant. Your job is to create an execution handoff for a coding agent that was interrupted mid-task.

CRITICAL REQUIREMENTS:
- Use EXACTLY these section headers (in English):
  * Current task:
  * User intent:
  * Repo / location:
  * Current state:
  * Important files:
  * Changes already made:
  * Known verification:
  * Unfinished work:
  * Next action:
  * Do not do:

- Be SPECIFIC and EXECUTABLE
- Include file paths, line numbers, command outputs
- "Next action" must be a single clear command or edit
- If you cannot determine the task, say "RECOVERY REQUIRED" explicitly

Quality rules:
- Preserve intent, not transcript
- Include only files directly relevant to the active task, each with a reason
- State "Unknown" explicitly instead of inventing
- Do not truncate mid-sentence
- Deduplicate file paths (prefer repo-relative)
- Make "Next action" a single executable step, not a category list`

// requiredHandoffSections are the section headers that must appear in a valid
// custom compact response. Used for output validation.
var requiredHandoffSections = []string{
	"Current task:",
	"User intent:",
	"Repo / location:",
	"Current state:",
	"Important files:",
	"Changes already made:",
	"Known verification:",
	"Unfinished work:",
	"Next action:",
	"Do not do:",
}

// customCompactRequest is the /chat/completions payload sent to the LLM.
type customCompactRequest struct {
	Model       string                   `json:"model"`
	Messages    []customCompactMessage   `json:"messages"`
	MaxTokens   int                      `json:"max_tokens,omitempty"`
	Temperature float64                  `json:"temperature"`
	Stream      bool                     `json:"stream"`
}

type customCompactMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// customCompactResponse is a minimal Responses API compact response envelope.
type customCompactResponse struct {
	ID        string                     `json:"id"`
	Object    string                     `json:"object"`
	CreatedAt int64                      `json:"created_at"`
	Status    string                     `json:"status"`
	Output    []customCompactOutputItem  `json:"output"`
}

type customCompactOutputItem struct {
	ID      string                       `json:"id"`
	Type    string                       `json:"type"`
	Role    string                       `json:"role"`
	Content []customCompactContentPart   `json:"content"`
}

type customCompactContentPart struct {
	Type        string   `json:"type"`
	Text        string   `json:"text"`
	Annotations []string `json:"annotations"`
}

// buildCompactResponseJSON creates a Responses API compact response with the
// given text as the sole output_text content.
func buildCompactResponseJSON(text string) ([]byte, error) {
	resp := customCompactResponse{
		ID:        fmt.Sprintf("resp_compact_%d", time.Now().UnixMilli()),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Output: []customCompactOutputItem{
			{
				ID:   "msg_compact_0",
				Type: "message",
				Role: "assistant",
				Content: []customCompactContentPart{
					{
						Type:        "output_text",
						Text:        text,
						Annotations: []string{},
					},
				},
			},
		},
	}
	return json.Marshal(resp)
}

// extractConversationForCompact extracts the conversation content from a
// compact request's input array and formats it as a text block suitable
// for LLM summarization. It skips reasoning items (which carry
// provider-specific signatures) and focuses on user/assistant messages.
func extractConversationForCompact(rawJSON []byte) string {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.Exists() {
		return ""
	}

	var sb strings.Builder

	// Handle input as a string (simple text input)
	if input.Type == gjson.String {
		sb.WriteString("User: ")
		sb.WriteString(input.String())
		return sb.String()
	}

	// Handle input as an array of items
	if !input.IsArray() {
		return ""
	}

	for _, item := range input.Array() {
		itemType := item.Get("type").String()
		role := item.Get("role").String()

		// Skip reasoning items - they carry provider-specific signatures
		if itemType == "reasoning" {
			continue
		}

		if itemType == "message" {
			// Extract text content from message items
			content := item.Get("content")
			if content.Type == gjson.String {
				sb.WriteString(capitalizeFirst(role))
				sb.WriteString(": ")
				sb.WriteString(content.String())
				sb.WriteString("\n\n")
				continue
			}
			if content.IsArray() {
				for _, part := range content.Array() {
					partType := part.Get("type").String()
					if partType == "input_text" || partType == "output_text" || partType == "text" {
						text := part.Get("text").String()
						if text != "" {
							sb.WriteString(capitalizeFirst(role))
							sb.WriteString(": ")
							sb.WriteString(text)
							sb.WriteString("\n\n")
						}
					}
				}
			}
			continue
		}

		// Handle other item types with content
		if itemType == "function_call" {
			name := item.Get("name").String()
			args := item.Get("arguments").String()
			sb.WriteString(fmt.Sprintf("Function call: %s(%s)\n\n", name, truncateText(args, 500)))
			continue
		}

		if itemType == "function_call_output" {
			output := item.Get("output").String()
			sb.WriteString(fmt.Sprintf("Function output: %s\n\n", truncateText(output, 500)))
			continue
		}
	}

	return sb.String()
}

// capitalizeFirst returns the string with its first character uppercased.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// truncateText truncates text to maxLen characters, appending "..." if truncated.
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// buildCustomCompactUserPrompt builds the user message for the LLM compaction
// call, including the extracted conversation and any additional context.
func buildCustomCompactUserPrompt(conversation string) string {
	var sb strings.Builder
	sb.WriteString("Create a compact execution handoff from this conversation context.\n\n")
	sb.WriteString("Conversation:\n")
	sb.WriteString(conversation)
	return sb.String()
}

// extractChatCompletionText extracts the assistant's text response from a
// /chat/completions response payload.
func extractChatCompletionText(payload []byte) string {
	// Standard chat completion format: choices[0].message.content
	content := gjson.GetBytes(payload, "choices.0.message.content")
	if content.Exists() && content.Type == gjson.String {
		return content.String()
	}
	return ""
}

// validateCustomCompactOutput checks whether the LLM compaction output
// contains all required handoff sections and meets minimum quality.
func validateCustomCompactOutput(text string) (ok bool, missing []string) {
	for _, section := range requiredHandoffSections {
		if !strings.Contains(text, section) {
			missing = append(missing, section)
		}
	}
	if len(text) < 200 {
		return false, missing
	}
	return len(missing) == 0, missing
}

// shouldApplyCustomCompact reports whether the custom compact path should be
// used for this request. It returns true when:
//   - custom-compact.enabled is true
//   - compact-fallback.enabled is false
//   - custom-compact.model is non-empty
//   - the requested model's providers match the custom compact criteria
//     (i.e., the model is NOT already served by a Codex provider that handles
//     compact natively)
func shouldApplyCustomCompact(h *OpenAIResponsesAPIHandler, modelName string) bool {
	if h == nil || h.Cfg == nil {
		return false
	}
	// Custom compact activates only when compact-fallback is disabled
	if h.Cfg.CompactFallback.Enabled {
		return false
	}
	cc := h.Cfg.CustomCompact
	if !cc.Enabled || cc.Model == "" {
		return false
	}
	// Skip if the original model is already served by a Codex provider
	// (native compact works without any custom handling)
	originalProviders := util.GetProviderName(modelName)
	if util.InArray(originalProviders, "codex") {
		return false
	}
	return true
}

// executeCustomCompact performs LLM-based context compaction:
// 1. Extracts conversation from the compact request input
// 2. Builds a system + user prompt for the LLM
// 3. Calls /chat/completions via ExecuteWithAuthManager with the configured model
// 4. Validates the output and retries if needed
// 5. Returns the result wrapped in the Responses API compact response format
func executeCustomCompact(h *OpenAIResponsesAPIHandler, cliCtx context.Context, modelName string, rawJSON []byte) ([]byte, error) {
	cc := h.Cfg.CustomCompact
	compactModel := cc.Model
	maxRetries := cc.EffectiveMaxRetries()
	maxTokens := cc.EffectiveMaxTokens()
	temperature := cc.EffectiveTemperature()

	conversation := extractConversationForCompact(rawJSON)
	if conversation == "" {
		log.Warn("custom compact: no conversation content extracted from input")
		conversation = "No conversation content available."
	}

	log.Infof("custom compact: using model %s for compaction (original model: %s)", compactModel, modelName)

	var lastText string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		userPrompt := buildCustomCompactUserPrompt(conversation)
		if attempt > 0 && lastText != "" {
			_, missing := validateCustomCompactOutput(lastText)
			userPrompt = fmt.Sprintf("Previous compact attempt was incomplete. Missing sections: %v\nPlease include ALL required sections.\n\n%s", missing, userPrompt)
		}

		chatReq := customCompactRequest{
			Model: compactModel,
			Messages: []customCompactMessage{
				{Role: "system", Content: customCompactSystemPrompt},
				{Role: "user", Content: userPrompt},
			},
			MaxTokens:   maxTokens,
			Temperature: temperature,
			Stream:      false,
		}

		chatPayload, err := json.Marshal(chatReq)
		if err != nil {
			return nil, fmt.Errorf("custom compact: marshal chat request: %w", err)
		}

		// Use the OpenAI chat handler type for the /chat/completions call
		resp, _, errMsg := h.ExecuteWithAuthManager(
			cliCtx,
			"openai",
			compactModel,
			chatPayload,
			"", // empty alt = /chat/completions path
		)
		if errMsg != nil {
			log.Warnf("custom compact attempt %d failed: %s", attempt, errMsg.Error)
			if attempt == maxRetries {
				return nil, fmt.Errorf("custom compact: LLM call failed after %d attempts: %s", attempt+1, errMsg.Error)
			}
			continue
		}

		text := extractChatCompletionText(resp)
		if text == "" {
			log.Warnf("custom compact attempt %d: empty response from LLM", attempt)
			lastText = ""
			if attempt == maxRetries {
				return nil, fmt.Errorf("custom compact: empty LLM response after %d attempts", attempt+1)
			}
			continue
		}

		lastText = text
		ok, missing := validateCustomCompactOutput(text)
		if ok || attempt == maxRetries {
			if !ok {
				log.Warnf("custom compact: accepting incomplete output after %d attempts (missing: %v)", attempt+1, missing)
			} else {
				log.Infof("custom compact: successful compaction on attempt %d, output length=%d", attempt+1, len(text))
			}
			return buildCompactResponseJSON(text)
		}

		log.Infof("custom compact attempt %d: output missing sections %v, retrying", attempt+1, missing)
	}

	// Should not reach here, but safety fallback
	return buildCompactResponseJSON(lastText)
}
