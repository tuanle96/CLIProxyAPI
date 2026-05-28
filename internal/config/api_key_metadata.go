package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const APIKeyStatusActive = "active"
const APIKeyStatusDisabled = "disabled"
const APIKeyStatusExpired = "expired"
const APIKeyStatusInvalid = "invalid"
const APIKeyStatusRevoked = "revoked"

const APIKeyQuotaPeriodDaily = "daily"
const APIKeyQuotaPeriodOneTime = "one_time"

// APIKeyAuditEvent records lifecycle changes that matter during credential reviews.
type APIKeyAuditEvent struct {
	Type    string `yaml:"type,omitempty" json:"type,omitempty"`
	At      string `yaml:"at,omitempty" json:"at,omitempty"`
	Message string `yaml:"message,omitempty" json:"message,omitempty"`
}

// APIKeyMetadata stores admin-facing lifecycle, ownership and policy metadata
// for top-level client API keys.
type APIKeyMetadata struct {
	Name             string             `yaml:"name,omitempty" json:"name,omitempty"`
	Owner            string             `yaml:"owner,omitempty" json:"owner,omitempty"`
	Environment      string             `yaml:"environment,omitempty" json:"environment,omitempty"`
	Description      string             `yaml:"description,omitempty" json:"description,omitempty"`
	Tags             []string           `yaml:"tags,omitempty" json:"tags,omitempty"`
	Scopes           []string           `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	AllowedProviders []string           `yaml:"allowed-providers,omitempty" json:"allowed-providers,omitempty"`
	AllowedModels    []string           `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`
	IPAllowlist      []string           `yaml:"ip-allowlist,omitempty" json:"ip-allowlist,omitempty"`
	CreatedAt        string             `yaml:"created-at,omitempty" json:"created-at,omitempty"`
	UpdatedAt        string             `yaml:"updated-at,omitempty" json:"updated-at,omitempty"`
	ExpiresAt        string             `yaml:"expires-at,omitempty" json:"expires-at,omitempty"`
	LastRotatedAt    string             `yaml:"last-rotated-at,omitempty" json:"last-rotated-at,omitempty"`
	Disabled         bool               `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	DisabledAt       string             `yaml:"disabled-at,omitempty" json:"disabled-at,omitempty"`
	Revoked          bool               `yaml:"revoked,omitempty" json:"revoked,omitempty"`
	RevokedAt        string             `yaml:"revoked-at,omitempty" json:"revoked-at,omitempty"`
	QuotaPeriod      string             `yaml:"quota-period,omitempty" json:"quota-period,omitempty"`
	TokenQuotaLimit  int64              `yaml:"token-quota-limit,omitempty" json:"token-quota-limit,omitempty"`
	USDQuotaLimit    float64            `yaml:"usd-quota-limit,omitempty" json:"usd-quota-limit,omitempty"`
	DailyTokenLimit  int64              `yaml:"daily-token-limit,omitempty" json:"daily-token-limit,omitempty"`
	MonthlyBudgetUSD float64            `yaml:"monthly-budget-usd,omitempty" json:"monthly-budget-usd,omitempty"`
	Audit            []APIKeyAuditEvent `yaml:"audit,omitempty" json:"audit,omitempty"`
}

// APIKeyID returns a stable non-secret identifier for a configured API key.
func APIKeyID(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MaskAPIKey returns a short display-safe fingerprint for a secret.
func MaskAPIKey(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 10 {
		return "****"
	}
	return trimmed[:6] + "..." + trimmed[len(trimmed)-4:]
}

func (m APIKeyMetadata) EffectiveStatus(now time.Time) (string, string) {
	if m.Revoked {
		return APIKeyStatusRevoked, "API key has been revoked"
	}
	if m.Disabled {
		return APIKeyStatusDisabled, "API key is disabled"
	}
	if _, ok := m.ExpiryTime(); !ok && strings.TrimSpace(m.ExpiresAt) != "" {
		return APIKeyStatusInvalid, "API key expires-at is invalid"
	}
	if expiry, ok := m.ExpiryTime(); ok && !now.Before(expiry) {
		return APIKeyStatusExpired, "API key is expired"
	}
	return APIKeyStatusActive, ""
}

func (m APIKeyMetadata) ExpiryTime() (time.Time, bool) {
	value := strings.TrimSpace(m.ExpiresAt)
	if value == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range formats {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// SanitizeAPIKeyMetadata normalizes metadata keys and prunes entries that do not
// correspond to the current top-level api-keys list.
func (cfg *Config) SanitizeAPIKeyMetadata() {
	if cfg == nil || len(cfg.APIKeyMetadata) == 0 {
		return
	}
	validIDs := make(map[string]struct{}, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		if id := APIKeyID(key); id != "" {
			validIDs[id] = struct{}{}
		}
	}
	next := make(map[string]APIKeyMetadata, len(cfg.APIKeyMetadata))
	for id, meta := range cfg.APIKeyMetadata {
		normalizedID := strings.TrimSpace(id)
		if _, ok := validIDs[normalizedID]; !ok {
			continue
		}
		next[normalizedID] = NormalizeAPIKeyMetadata(meta)
	}
	if len(next) == 0 {
		cfg.APIKeyMetadata = nil
		return
	}
	cfg.APIKeyMetadata = next
}

func NormalizeAPIKeyMetadata(meta APIKeyMetadata) APIKeyMetadata {
	meta.Name = strings.TrimSpace(meta.Name)
	meta.Owner = strings.TrimSpace(meta.Owner)
	meta.Environment = strings.TrimSpace(meta.Environment)
	meta.Description = strings.TrimSpace(meta.Description)
	meta.CreatedAt = strings.TrimSpace(meta.CreatedAt)
	meta.UpdatedAt = strings.TrimSpace(meta.UpdatedAt)
	meta.ExpiresAt = strings.TrimSpace(meta.ExpiresAt)
	meta.LastRotatedAt = strings.TrimSpace(meta.LastRotatedAt)
	meta.DisabledAt = strings.TrimSpace(meta.DisabledAt)
	meta.RevokedAt = strings.TrimSpace(meta.RevokedAt)
	meta.QuotaPeriod = normalizeQuotaPeriod(meta.QuotaPeriod)
	meta.Tags = normalizeStringSlice(meta.Tags)
	meta.Scopes = normalizeStringSlice(meta.Scopes)
	meta.AllowedProviders = normalizeStringSlice(meta.AllowedProviders)
	meta.AllowedModels = normalizeStringSlice(meta.AllowedModels)
	meta.IPAllowlist = normalizeStringSlice(meta.IPAllowlist)
	if meta.TokenQuotaLimit < 0 {
		meta.TokenQuotaLimit = 0
	}
	if meta.USDQuotaLimit < 0 {
		meta.USDQuotaLimit = 0
	}
	if meta.DailyTokenLimit < 0 {
		meta.DailyTokenLimit = 0
	}
	if meta.MonthlyBudgetUSD < 0 {
		meta.MonthlyBudgetUSD = 0
	}
	if meta.TokenQuotaLimit == 0 && meta.DailyTokenLimit > 0 {
		meta.TokenQuotaLimit = meta.DailyTokenLimit
		if meta.QuotaPeriod == "" {
			meta.QuotaPeriod = APIKeyQuotaPeriodDaily
		}
	}
	if meta.USDQuotaLimit == 0 && meta.MonthlyBudgetUSD > 0 {
		meta.USDQuotaLimit = meta.MonthlyBudgetUSD
		if meta.QuotaPeriod == "" {
			meta.QuotaPeriod = APIKeyQuotaPeriodOneTime
		}
	}
	if meta.QuotaPeriod == "" && (meta.TokenQuotaLimit > 0 || meta.USDQuotaLimit > 0) {
		meta.QuotaPeriod = APIKeyQuotaPeriodOneTime
	}
	if len(meta.Audit) > 0 {
		audit := make([]APIKeyAuditEvent, 0, len(meta.Audit))
		for _, event := range meta.Audit {
			event.Type = strings.TrimSpace(event.Type)
			event.At = strings.TrimSpace(event.At)
			event.Message = strings.TrimSpace(event.Message)
			if event.Type == "" && event.At == "" && event.Message == "" {
				continue
			}
			audit = append(audit, event)
		}
		meta.Audit = audit
	}
	return meta
}

func normalizeQuotaPeriod(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", APIKeyQuotaPeriodDaily, APIKeyQuotaPeriodOneTime:
		return normalized
	default:
		return normalized
	}
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
