// Package common — helpers for keeping the Kiro conversationId stable across
// the multi-turn requests a single client thread fires. See the call sites in
// internal/translator/kiro/openai and internal/translator/kiro/claude for
// context. Sharing one implementation avoids drift between the two payload
// builders that both need the same stability guarantee.
package common

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// stableConversationIDHeaders lists the request headers known to carry stable
// per-thread identifiers across the clients we proxy for. We try them in
// priority order; the first non-empty value wins. Codex Desktop and the
// Codex CLI both ship the lower three; LangChain/LlamaIndex agents commonly
// use the upper ones.
var stableConversationIDHeaders = []string{
	"Conversation-Id",
	"X-Conversation-Id",
	"Thread-Id",
	"X-Codex-Thread-Id",
	"Session-Id",
	"X-Codex-Window-Id",
	"X-Client-Request-Id",
}

// DeriveStableConversationID returns a UUID-shaped identifier that is stable
// for a single client thread, derived (in priority order) from request
// headers or the first user message in the prompt. Returning an empty string
// means the caller should fall back to a random UUID.
//
// Output format: 8-4-4-4-12 hex (UUID-shaped) so it slots into Kiro's
// conversationId field without surprises, but it is NOT a registered UUID —
// just a deterministic 16-byte SHA-1 prefix expressed in UUID layout.
func DeriveStableConversationID(headers http.Header, messages gjson.Result) string {
	if seed := headerStableSeed(headers); seed != "" {
		return shapeAsUUID(sha1Hex(seed))
	}
	if seed := firstUserMessageSeed(messages); seed != "" {
		return shapeAsUUID(sha1Hex(seed))
	}
	return ""
}

// headerStableSeed picks the first non-empty stable-identifier header from
// the well-known set above, lower-cased and trimmed.
func headerStableSeed(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, name := range stableConversationIDHeaders {
		if v := strings.TrimSpace(headers.Get(name)); v != "" {
			// Strip optional `:N` suffix Codex Desktop appends to window ids
			// (e.g. "019e...:0") so the same logical thread keeps one id even
			// across window indexes.
			if idx := strings.IndexByte(v, ':'); idx > 0 {
				v = v[:idx]
			}
			return strings.ToLower(name) + "=" + v
		}
	}
	return ""
}

// firstUserMessageSeed walks the messages array and returns a seed string
// derived from the first user-role message's text content. The seed includes
// the role name so that prompts that happen to share text but differ in
// origin still get different conversation ids.
func firstUserMessageSeed(messages gjson.Result) string {
	if !messages.IsArray() {
		return ""
	}
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		var text string
		switch {
		case content.Type == gjson.String:
			text = content.String()
		case content.IsArray():
			parts := make([]string, 0, 4)
			content.ForEach(func(_, item gjson.Result) bool {
				if t := item.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
				return true
			})
			text = strings.Join(parts, "\n")
		}
		text = strings.TrimSpace(text)
		if text != "" {
			return "first_user=" + text
		}
	}
	return ""
}

// sha1Hex returns the lower-case hex SHA-1 of seed.
func sha1Hex(seed string) string {
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// shapeAsUUID slots a >=32-hex string into the canonical 8-4-4-4-12 UUID
// layout. The result is not RFC-4122 compliant (we don't set version/variant
// bits) — it is purely a textual shape Kiro accepts as a conversationId.
func shapeAsUUID(hexStr string) string {
	if len(hexStr) < 32 {
		hexStr = hexStr + strings.Repeat("0", 32-len(hexStr))
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexStr[0:8],
		hexStr[8:12],
		hexStr[12:16],
		hexStr[16:20],
		hexStr[20:32],
	)
}
