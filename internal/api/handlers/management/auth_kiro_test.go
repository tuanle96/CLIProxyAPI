package management

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// runKiroAuthRequest invokes the Kiro auth-url handler against an in-memory gin
// test recorder. It bypasses authentication middleware on purpose — these tests
// only exercise the handler's parameter validation, not the auth chain — so
// they assert how the handler reacts to query input alone.
func runKiroAuthRequest(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	parsedURL, err := url.Parse("http://test/" + query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/kiro-auth-url?"+parsedURL.RawQuery, nil)

	h.RequestKiroToken(c)
	return rec
}

func TestRequestKiroToken_RejectsUnknownMethod(t *testing.T) {
	rec := runKiroAuthRequest(t, "?method=foo")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown method, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown method") {
		t.Fatalf("expected error body to mention unknown method, got %s", rec.Body.String())
	}
}

func TestRequestKiroToken_IDCRequiresStartURL(t *testing.T) {
	rec := runKiroAuthRequest(t, "?method=idc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for IDC without start_url, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "start_url") {
		t.Fatalf("expected error body to mention start_url, got %s", rec.Body.String())
	}
}

func TestRequestKiroToken_IDCWithStartURLDoesNotShortCircuitTo400(t *testing.T) {
	// With both required IDC params present, validation must pass — the handler
	// then makes a real network call to AWS SSO OIDC. We don't want that in unit
	// tests, so we accept any non-4xx status. In practice this returns 5xx
	// because there is no live AWS connection in the test env, but never 4xx
	// from validation.
	rec := runKiroAuthRequest(t, "?method=idc&start_url=https://example.awsapps.com/start&region=us-east-1")
	if rec.Code >= 400 && rec.Code < 500 {
		t.Fatalf("expected validation to pass, got 4xx %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestRequestKiroToken_BuilderIDDefaultDoesNotShortCircuitTo400(t *testing.T) {
	// No params at all — handler defaults to method=builder-id, which is valid
	// without any extra inputs. Validation must pass; the AWS network call may
	// fail in offline test envs.
	rec := runKiroAuthRequest(t, "")
	if rec.Code >= 400 && rec.Code < 500 {
		t.Fatalf("expected validation to pass for default builder-id, got 4xx %d (body=%s)", rec.Code, rec.Body.String())
	}
}
