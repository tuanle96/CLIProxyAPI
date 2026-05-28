package apikeypolicy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestValidateMetadataRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		meta internalconfig.APIKeyMetadata
	}{
		{name: "expiry", meta: internalconfig.APIKeyMetadata{ExpiresAt: "tomorrow"}},
		{name: "period", meta: internalconfig.APIKeyMetadata{QuotaPeriod: "monthly"}},
		{name: "negative token", meta: internalconfig.APIKeyMetadata{TokenQuotaLimit: -1}},
		{name: "negative usd", meta: internalconfig.APIKeyMetadata{USDQuotaLimit: -1}},
		{name: "model wildcard", meta: internalconfig.APIKeyMetadata{AllowedModels: []string{"gpt-*bad*"}}},
		{name: "provider wildcard", meta: internalconfig.APIKeyMetadata{AllowedProviders: []string{"open*"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateMetadata(tc.meta); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCheckRequestAllowsWildcardModelAndFiltersProviders(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	key := "sk-policy-test"
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{
			internalconfig.APIKeyID(key): {
				AllowedModels:    []string{"gpt-5*"},
				AllowedProviders: []string{"openai"},
			},
		},
	}
	providers, errMsg := CheckRequest(cfg, key, "openai", []string{"openai", "claude"}, "openai/gpt-5.2(high)", "openai/gpt-5.2(high)", time.Now())
	if errMsg != nil {
		t.Fatalf("CheckRequest returned error: %v", errMsg.Error)
	}
	if len(providers) != 1 || providers[0] != "openai" {
		t.Fatalf("providers = %#v, want openai only", providers)
	}

	_, errMsg = CheckRequest(cfg, key, "openai", []string{"openai"}, "claude-sonnet-4-5", "claude-sonnet-4-5", time.Now())
	if errMsg == nil || errMsg.StatusCode != 403 {
		t.Fatalf("expected forbidden model error, got %#v", errMsg)
	}
}

func TestQuotaDailyBlocksAndResetsNextLocalDay(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	key := "sk-quota-daily"
	meta := internalconfig.APIKeyMetadata{
		QuotaPeriod:     internalconfig.APIKeyQuotaPeriodDaily,
		TokenQuotaLimit: 100,
	}
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{
			internalconfig.APIKeyID(key): meta,
		},
	}
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.Local)
	defaultManager.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      key,
		Provider:    "openai",
		Model:       "gpt-5",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: 100},
	})

	_, errMsg := CheckRequest(cfg, key, "openai", []string{"openai"}, "gpt-5", "gpt-5", now)
	if errMsg == nil || errMsg.StatusCode != 403 {
		t.Fatalf("expected quota block, got %#v", errMsg)
	}

	_, errMsg = CheckRequest(cfg, key, "openai", []string{"openai"}, "gpt-5", "gpt-5", now.Add(24*time.Hour))
	if errMsg != nil {
		t.Fatalf("expected next local day to reset daily quota, got %v", errMsg.Error)
	}
}

func TestQuotaOneTimePersistsAcrossFileLedgerRestart(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	dir := t.TempDir()
	if err := ConfigureFileLedger(dir); err != nil {
		t.Fatalf("ConfigureFileLedger: %v", err)
	}
	key := "sk-quota-file"
	meta := internalconfig.APIKeyMetadata{
		QuotaPeriod:     internalconfig.APIKeyQuotaPeriodOneTime,
		TokenQuotaLimit: 10,
	}
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.Local)
	defaultManager.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      key,
		Provider:    "openai",
		Model:       "gpt-5",
		RequestedAt: now,
		Detail:      coreusage.Detail{TotalTokens: 10},
	})
	if _, err := StatusForAPIKey(key, meta, now); err != nil {
		t.Fatalf("status before restart: %v", err)
	}

	if err := ConfigureFileLedger(dir); err != nil {
		t.Fatalf("ConfigureFileLedger restart: %v", err)
	}
	status, err := StatusForAPIKey(key, meta, now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("status after restart: %v", err)
	}
	if !status.Blocked || status.TokenQuota.Used != 10 {
		t.Fatalf("status = %#v, want persisted blocked one-time usage", status)
	}
	if _, err := filepath.Abs(filepath.Join(dir, "quota-ledger.json")); err != nil {
		t.Fatalf("ledger path sanity check: %v", err)
	}
}

func TestUSDQuotaUsesUsagePortalCostEstimate(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	key := "sk-quota-usd"
	meta := internalconfig.APIKeyMetadata{
		QuotaPeriod:   internalconfig.APIKeyQuotaPeriodOneTime,
		USDQuotaLimit: 3,
	}
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{
			internalconfig.APIKeyID(key): meta,
		},
	}
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.Local)
	defaultManager.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      key,
		Provider:    "openai",
		Model:       "gpt-5",
		RequestedAt: now,
		Detail:      coreusage.Detail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})

	status, err := StatusForAPIKey(key, meta, now)
	if err != nil {
		t.Fatalf("StatusForAPIKey: %v", err)
	}
	if status.USDQuota.Used != 3 || !status.USDQuota.Exceeded {
		t.Fatalf("usd quota status = %#v, want $3 exhausted", status.USDQuota)
	}
	_, errMsg := CheckRequest(cfg, key, "openai", []string{"openai"}, "gpt-5", "gpt-5", now)
	if errMsg == nil || errMsg.StatusCode != 403 {
		t.Fatalf("expected USD quota block, got %#v", errMsg)
	}
}

func TestPostgresLedgerUpsertAndRead(t *testing.T) {
	dsn := os.Getenv("USAGEPORTAL_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set USAGEPORTAL_POSTGRES_TEST_DSN to run postgres integration test")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("apikeypolicy_test_%d", time.Now().UnixNano())
	ledger, err := newPostgresLedger(ctx, PostgresLedgerConfig{DSN: dsn, Schema: schema})
	if err != nil {
		t.Fatalf("newPostgresLedger: %v", err)
	}
	defer ledger.Close()
	defer func() {
		if _, errDrop := ledger.db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", quoteSQLIdentifier(schema))); errDrop != nil {
			t.Fatalf("drop test schema: %v", errDrop)
		}
	}()

	key := LedgerKey{APIKeyHash: "sha256:test", Period: internalconfig.APIKeyQuotaPeriodOneTime, PeriodKey: oneTimePeriodKey}
	if err = ledger.Add(ctx, key, QuotaUsage{TotalTokens: 7, CostUSD: 0.5, Requests: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err = ledger.Add(ctx, key, QuotaUsage{TotalTokens: 3, CostUSD: 0.25, Requests: 2, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("second add: %v", err)
	}
	usage, err := ledger.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if usage.TotalTokens != 10 || usage.CostUSD != 0.75 || usage.Requests != 3 {
		t.Fatalf("usage = %#v, want accumulated upsert values", usage)
	}
}


func TestFilterAllowedModelsRespectsKeyMetadata(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	key := "sk-filter-test"
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{
			internalconfig.APIKeyID(key): {
				AllowedModels: []string{"deepseek*", "mimo*"},
			},
		},
	}

	models := []map[string]any{
		{"id": "deepseek-v3"},
		{"id": "deepseek-r1"},
		{"id": "mimo-v2.5-tts"},
		{"id": "claude-sonnet-4-5"},
		{"id": "gpt-5.2"},
		{"id": "kimi-k2.6"},
	}

	got := FilterAllowedModels(cfg, key, models)
	if len(got) != 3 {
		t.Fatalf("expected 3 models after filter, got %d: %#v", len(got), got)
	}
	wantIDs := map[string]bool{"deepseek-v3": true, "deepseek-r1": true, "mimo-v2.5-tts": true}
	for _, entry := range got {
		id, _ := entry["id"].(string)
		if !wantIDs[id] {
			t.Fatalf("unexpected model in filter output: %s", id)
		}
	}
}

func TestFilterAllowedModelsPassthroughWithoutPolicy(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	models := []map[string]any{{"id": "claude-opus-4-5"}, {"id": "gpt-5.2"}}

	// No cfg / no key — return input untouched.
	if got := FilterAllowedModels(nil, "any", models); len(got) != 2 {
		t.Fatalf("nil cfg should return input unchanged, got %d", len(got))
	}
	if got := FilterAllowedModels(&sdkconfig.SDKConfig{}, "", models); len(got) != 2 {
		t.Fatalf("empty key should return input unchanged, got %d", len(got))
	}

	// Key exists but no allowed-models metadata — passthrough.
	key := "sk-no-policy"
	cfg := &sdkconfig.SDKConfig{
		APIKeys:        []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{internalconfig.APIKeyID(key): {}},
	}
	if got := FilterAllowedModels(cfg, key, models); len(got) != 2 {
		t.Fatalf("key without allowed-models should passthrough, got %d", len(got))
	}

	// Unknown key (not registered in cfg.APIKeys) — passthrough so an in-flight
	// rotation cannot accidentally hide the catalog from a still-valid key.
	if got := FilterAllowedModels(cfg, "sk-unknown", models); len(got) != 2 {
		t.Fatalf("unknown key should passthrough, got %d", len(got))
	}
}

func TestFilterAllowedModelsHandlesGeminiAndCodexShapes(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	key := "sk-shapes"
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{key},
		APIKeyMetadata: map[string]internalconfig.APIKeyMetadata{
			internalconfig.APIKeyID(key): {AllowedModels: []string{"gemini-2.5*", "gpt-5.2"}},
		},
	}

	models := []map[string]any{
		{"name": "models/gemini-2.5-pro"},      // Gemini-style with prefix
		{"name": "models/gemini-1.5-flash"},    // does not match
		{"slug": "gpt-5.2"},                    // Codex client style
		{"slug": "gpt-5.4-mini"},               // does not match
		{"id": "deepseek-v3"},                  // does not match
		{"id": "", "name": "", "slug": ""},     // empty identifiers — dropped
		{"description": "no identifier field"}, // missing identifier — dropped
	}

	got := FilterAllowedModels(cfg, key, models)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(got), got)
	}
}
