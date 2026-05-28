// Package main is a single-binary build of CLIProxyAPI augmented with a
// Gin middleware that adds vision support to text-only models on the fly.
//
// For every POST /v1/chat/completions, /v1/responses or /v1/messages request
// containing image content, the middleware:
//
//  1. Decompresses the body (gzip / brotli / zstd are supported, identity is
//     passed through).
//  2. Calls a vision-capable model (default: kimi-k2.6) to describe each
//     image. Descriptions are cached on disk by sha256 of the image data URI
//     so identical images in follow-up turns aren't described again.
//  3. Replaces every image part in the request with a text part containing
//     "[Image Description: …]" and forwards the rewritten body upstream.
//     The original "model" field is preserved — the user-chosen model
//     (e.g. deepseek-v4-pro) keeps answering normally.
//
// Caption calls are issued as HTTP loopback to the same server. They carry
// the X-Vision-Internal: 1 header which the middleware recognises and skips,
// so they cannot recurse.
//
// Configuration:
//
//	./cli-proxy-api-vision --config /path/to/config.yaml
//
// All CLIProxyAPI flags work the same. Vision-specific knobs come from env:
//
//	CAPTION_MODEL         default kimi-k2.6
//	CAPTION_API_BASE      default http://<host>:<port>/v1 (this server)
//	CAPTION_API_KEY       default first proxy api-key from config
//	CAPTION_PROMPT        default: built-in coding-agent prompt
//	CAPTION_MAX_TOKENS    default 800
//	CAPTION_TIMEOUT       default 90s
//	CAPTION_CACHE_DIR     default $TMPDIR/cli-proxy-api-vision-cache
//	CAPTION_CONCURRENCY   default 4
//	VISION_DISABLE        when "1", middleware is bypassed entirely
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	visionInternalHeader = "X-Vision-Internal"
	visionInternalValue  = "1"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to CLIProxyAPI config file")
	flag.Parse()

	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Fatalf("resolve config path: %v", err)
	}

	cfg, err := config.LoadConfig(abs)
	if err != nil {
		log.Fatalf("load config %s: %v", abs, err)
	}

	cc := loadCaptionConfig(cfg)
	log.Printf("[vision] model=%s base=%s prompt=%dB max_tokens=%d cache=%s",
		cc.Model, cc.UpstreamBase, len(cc.Prompt), cc.MaxTokens, cc.CacheDir)

	mw := newVisionMiddleware(cc)

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(abs).
		WithServerOptions(api.WithMiddleware(mw)).
		Build()
	if err != nil {
		log.Fatalf("build service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("[vision] caught signal %s — shutting down", s)
		cancel()
	}()

	host := cfg.Host
	if host == "" {
		host = "0.0.0.0"
	}
	log.Printf("[vision] cli-proxy-api-vision listening on %s:%d", host, cfg.Port)

	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("run: %v", err)
	}
}

func newVisionMiddleware(cc captionConfig) gin.HandlerFunc {
	visionPaths := map[string]bool{
		"/v1/chat/completions": true,
		"/v1/responses":        true,
		"/v1/messages":         true,
	}
	disabled := os.Getenv("VISION_DISABLE") == "1"

	return func(c *gin.Context) {
		if disabled || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		// Loopback caption calls carry this header; never recurse.
		if c.GetHeader(visionInternalHeader) == visionInternalValue {
			c.Next()
			return
		}
		if !visionPaths[c.Request.URL.Path] {
			c.Next()
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 50<<20))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}
		_ = c.Request.Body.Close()

		ce := c.GetHeader("Content-Encoding")
		decoded, derr := decompress(body, ce)
		if derr != nil {
			log.Printf("[vision] decompress(%s) on %s failed: %v — passthrough",
				ce, c.Request.URL.Path, derr)
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}

		var doc map[string]any
		if err := json.Unmarshal(decoded, &doc); err != nil {
			// Not JSON: forward original.
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}

		refs := collectImages(doc)
		if len(refs) == 0 {
			// No images: forward original (preserve compression).
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}

		cached, described := 0, 0
		for _, r := range refs {
			if _, ok := readCache(cc.CacheDir, r.hash); ok {
				cached++
			} else {
				described++
			}
		}

		describeAll(c.Request.Context(), refs, cc, log.Printf)

		newBody, jerr := json.Marshal(doc)
		if jerr != nil {
			log.Printf("[vision] re-marshal failed on %s: %v — passthrough",
				c.Request.URL.Path, jerr)
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}

		log.Printf("[vision] %s images=%d cache_hits=%d (decoded %d -> forwarded %d, encoding=%q)",
			c.Request.URL.Path, cached+described, cached, len(decoded), len(newBody), ce)

		c.Request.Header.Del("Content-Encoding")
		c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
		c.Request.ContentLength = int64(len(newBody))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		c.Next()
	}
}

// loadCaptionConfig reads vision knobs from env, defaulting to safe values.
// The loopback URL is built from cfg.Host/cfg.Port so caption calls hit this
// very server (and therefore go through the same OpenAI-compat upstream).
func loadCaptionConfig(cfg *config.Config) captionConfig {
	host := cfg.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	defaultBase := "http://" + host + ":" + strconv.Itoa(cfg.Port) + "/v1"

	apiKey := os.Getenv("CAPTION_API_KEY")
	if apiKey == "" && len(cfg.APIKeys) > 0 {
		apiKey = cfg.APIKeys[0]
	}

	timeout, perr := time.ParseDuration(envOr("CAPTION_TIMEOUT", "90s"))
	if perr != nil || timeout <= 0 {
		timeout = 90 * time.Second
	}

	return captionConfig{
		UpstreamBase:   envOr("CAPTION_API_BASE", defaultBase),
		Model:          envOr("CAPTION_MODEL", "kimi-k2.6"),
		APIKey:         apiKey,
		Prompt:         envOr("CAPTION_PROMPT", defaultCaptionPrompt),
		MaxTokens:      atoi(os.Getenv("CAPTION_MAX_TOKENS"), 800),
		CacheDir:       envOr("CAPTION_CACHE_DIR", filepath.Join(os.TempDir(), "cli-proxy-api-vision-cache")),
		Timeout:        timeout,
		Concurrency:    atoi(os.Getenv("CAPTION_CONCURRENCY"), 4),
		InternalHeader: visionInternalHeader,
		InternalValue:  visionInternalValue,
	}
}

const defaultCaptionPrompt = "You are an image-description assistant for a coding agent. " +
	"Describe the image as concretely and faithfully as possible: " +
	"identify text content (transcribe verbatim when readable), UI elements, " +
	"diagrams, code, charts, screenshots, error messages, and spatial layout. " +
	"Avoid speculation; if something is unclear, say so. " +
	"Be thorough but use plain prose; no markdown code fences."

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// decompress decodes a request body according to the Content-Encoding header.
// Supported: identity (empty), gzip, br (brotli), zstd. Unknown encodings
// return the original bytes and no error so the caller can forward as-is.
func decompress(body []byte, enc string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "identity":
		return body, nil
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return io.ReadAll(gr)
	case "br":
		return io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	default:
		return body, nil
	}
}
