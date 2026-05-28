package config

import "testing"

func TestParseConfigBytesLoadsAPIKeyMetadata(t *testing.T) {
	t.Parallel()

	key := "sk-config-key"
	id := APIKeyID(key)
	cfg, err := ParseConfigBytes([]byte(`
api-keys:
  - sk-config-key
api-key-metadata:
  ` + id + `:
    name: CI runner
    owner: platform
    environment: prod
    scopes:
      - chat
      - responses
    ip-allowlist:
      - 127.0.0.1
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	meta := cfg.APIKeyMetadata[id]
	if meta.Name != "CI runner" || meta.Owner != "platform" || meta.Environment != "prod" {
		t.Fatalf("metadata = %#v", meta)
	}
	if len(meta.Scopes) != 2 {
		t.Fatalf("scopes = %#v", meta.Scopes)
	}
	if len(meta.IPAllowlist) != 1 || meta.IPAllowlist[0] != "127.0.0.1" {
		t.Fatalf("ip allowlist = %#v", meta.IPAllowlist)
	}
}

func TestSanitizeAPIKeyMetadataPrunesMissingKeys(t *testing.T) {
	t.Parallel()

	kept := "sk-kept"
	removed := "sk-removed"
	cfg := &Config{SDKConfig: SDKConfig{
		APIKeys: []string{kept},
		APIKeyMetadata: map[string]APIKeyMetadata{
			APIKeyID(kept):    {Owner: "platform"},
			APIKeyID(removed): {Owner: "stale"},
		},
	}}

	cfg.SanitizeAPIKeyMetadata()

	if _, ok := cfg.APIKeyMetadata[APIKeyID(kept)]; !ok {
		t.Fatal("expected kept metadata to remain")
	}
	if _, ok := cfg.APIKeyMetadata[APIKeyID(removed)]; ok {
		t.Fatal("expected stale metadata to be pruned")
	}
}

func TestNormalizeAPIKeyMetadataMapsLegacyQuotaFields(t *testing.T) {
	t.Parallel()

	meta := NormalizeAPIKeyMetadata(APIKeyMetadata{
		DailyTokenLimit:  1_000_000,
		MonthlyBudgetUSD: 25,
	})

	if meta.QuotaPeriod != APIKeyQuotaPeriodDaily {
		t.Fatalf("quota period = %q, want daily", meta.QuotaPeriod)
	}
	if meta.TokenQuotaLimit != 1_000_000 {
		t.Fatalf("token quota = %d, want 1000000", meta.TokenQuotaLimit)
	}
	if meta.USDQuotaLimit != 25 {
		t.Fatalf("usd quota = %f, want 25", meta.USDQuotaLimit)
	}
}
