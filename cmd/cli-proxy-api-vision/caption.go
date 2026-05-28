package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// imgRef points at a single image part inside a parsed JSON document so we
// can mutate it in place after captioning.
//
// Three shapes are supported:
//
//  1. OpenAI Responses API (Codex CLI):
//     {"input":[ {"role":"user","content":[
//        {"type":"input_image","image_url":"data:image/...","detail":"high"}
//     ]} ]}
//
//  2. OpenAI chat/completions:
//     {"messages":[ {"role":"user","content":[
//        {"type":"image_url","image_url":{"url":"data:image/..."}}
//     ]} ]}
//
//  3. Anthropic messages:
//     {"messages":[ {"role":"user","content":[
//        {"type":"image","source":{"type":"base64","data":"...","media_type":"image/png"}}
//     ]} ]}
type imgRef struct {
	parent  map[string]any
	dataURI string
	hash    string
}

func collectImages(doc map[string]any) []*imgRef {
	var out []*imgRef
	visitArr := func(arr []any) {
		for _, m := range arr {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if uri := imageDataURI(mm); uri != "" {
				out = append(out, &imgRef{parent: mm, dataURI: uri})
			}
			if c, ok := mm["content"].([]any); ok {
				for _, p := range c {
					pp, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if uri := imageDataURI(pp); uri != "" {
						out = append(out, &imgRef{parent: pp, dataURI: uri})
					}
				}
			}
		}
	}
	if a, ok := doc["messages"].([]any); ok {
		visitArr(a)
	}
	if a, ok := doc["input"].([]any); ok {
		visitArr(a)
	}
	for _, ref := range out {
		sum := sha256.Sum256([]byte(ref.dataURI))
		ref.hash = hex.EncodeToString(sum[:])
	}
	return out
}

func imageDataURI(part map[string]any) string {
	t, _ := part["type"].(string)
	switch t {
	case "input_image":
		if s, ok := part["image_url"].(string); ok && s != "" {
			return s
		}
		if obj, ok := part["image_url"].(map[string]any); ok {
			if u, _ := obj["url"].(string); u != "" {
				return u
			}
		}
	case "image_url":
		if obj, ok := part["image_url"].(map[string]any); ok {
			if u, _ := obj["url"].(string); u != "" {
				return u
			}
		}
		if s, ok := part["image_url"].(string); ok && s != "" {
			return s
		}
	case "image":
		if src, ok := part["source"].(map[string]any); ok {
			if u, _ := src["url"].(string); u != "" {
				return u
			}
			if data, _ := src["data"].(string); data != "" {
				media, _ := src["media_type"].(string)
				if media == "" {
					media = "image/png"
				}
				return "data:" + media + ";base64," + data
			}
		}
	}
	return ""
}

func replaceWithCaption(part map[string]any, description string) {
	t, _ := part["type"].(string)
	delete(part, "image_url")
	delete(part, "source")
	delete(part, "detail")
	switch t {
	case "input_image":
		part["type"] = "input_text"
		part["text"] = description
	case "image":
		part["type"] = "text"
		part["text"] = description
	default:
		part["type"] = "text"
		part["text"] = description
	}
}

// captionConfig is everything the caption pipeline needs.
type captionConfig struct {
	UpstreamBase string
	Model        string
	APIKey       string
	Prompt       string
	MaxTokens    int
	CacheDir     string
	Timeout      time.Duration
	Concurrency  int

	// InternalHeader/Value are added to every loopback caption call so the
	// vision middleware on the receiving side can detect and bypass it.
	InternalHeader string
	InternalValue  string
}

// describeAll captions every image referenced in refs, mutating their parent
// JSON nodes in place. Cached descriptions are reused. Errors for individual
// images become placeholder text so the upstream model still receives valid
// content.
func describeAll(ctx context.Context, refs []*imgRef, cc captionConfig, logf func(string, ...any)) {
	if len(refs) == 0 {
		return
	}
	type result struct {
		idx  int
		text string
		err  error
	}
	conc := cc.Concurrency
	if conc <= 0 {
		conc = 4
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	results := make([]result, len(refs))
	for i, r := range refs {
		i, r := i, r
		if cached, ok := readCache(cc.CacheDir, r.hash); ok {
			results[i] = result{idx: i, text: cached}
			logf("[vision] cache hit %s (%d bytes desc)", r.hash[:12], len(cached))
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			text, err := callVision(ctx, r.dataURI, cc)
			if err != nil {
				logf("[vision] describe %s failed in %s: %v", r.hash[:12], time.Since(start), err)
				results[i] = result{idx: i, err: err}
				return
			}
			logf("[vision] described %s in %s (%d chars)", r.hash[:12], time.Since(start), len(text))
			results[i] = result{idx: i, text: text}
			writeCache(cc.CacheDir, r.hash, text)
		}()
	}
	wg.Wait()

	for i, r := range refs {
		res := results[i]
		var desc string
		if res.err != nil {
			desc = fmt.Sprintf("[Image Description Unavailable: %v]", sanitizeErr(res.err))
		} else {
			desc = "[Image Description: " + strings.TrimSpace(res.text) + "]"
		}
		replaceWithCaption(r.parent, desc)
	}
}

func sanitizeErr(err error) string {
	s := err.Error()
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

// callVision posts a chat/completions describe call to the loopback URL,
// authenticated with the proxy API key, marked with InternalHeader so the
// in-process vision middleware passes it through without re-captioning.
func callVision(ctx context.Context, dataURI string, cc captionConfig) (string, error) {
	if cc.UpstreamBase == "" || cc.Model == "" {
		return "", errors.New("vision config not set")
	}
	body := map[string]any{
		"model":      cc.Model,
		"max_tokens": cc.MaxTokens,
		"stream":     false,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": cc.Prompt},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": dataURI, "detail": "high"},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(cc.UpstreamBase, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cc.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cc.APIKey)
	}
	if cc.InternalHeader != "" {
		req.Header.Set(cc.InternalHeader, cc.InternalValue)
	}
	cli := &http.Client{Timeout: cc.Timeout}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(respBody), 400))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   any    `json:"content"`
				Reasoning string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w (raw=%s)", err, truncate(string(respBody), 300))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("empty choices: %s", truncate(string(respBody), 300))
	}
	c := parsed.Choices[0].Message.Content
	text := flattenContent(c)
	if strings.TrimSpace(text) == "" {
		text = parsed.Choices[0].Message.Reasoning
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty content: %s", truncate(string(respBody), 300))
	}
	return text, nil
}

func flattenContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, p := range x {
			pp, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pp["text"].(string); t != "" {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...<truncated>"
}

// --- file cache --------------------------------------------------------------

func cachePath(dir, hash string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, hash+".txt")
}

func readCache(dir, hash string) (string, bool) {
	p := cachePath(dir, hash)
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func writeCache(dir, hash, value string) {
	p := cachePath(dir, hash)
	if p == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
