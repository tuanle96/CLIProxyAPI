package apikeypolicy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const errorCodePolicyViolation = "api_key_policy_violation"
const errorCodeQuotaStoreUnavailable = "quota_store_unavailable"

type policyError struct {
	StatusCode int
	Code       string
	Type       string
	Message    string
}

func (e policyError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.StatusCode)
}

func (e policyError) errorMessage() *interfaces.ErrorMessage {
	if e.StatusCode <= 0 {
		e.StatusCode = http.StatusForbidden
	}
	if e.Type == "" {
		if e.StatusCode >= http.StatusInternalServerError {
			e.Type = "server_error"
		} else {
			e.Type = "permission_error"
		}
	}
	if e.Code == "" {
		e.Code = errorCodePolicyViolation
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.Error(),
			"type":    e.Type,
			"code":    e.Code,
		},
	})
	if err != nil {
		return &interfaces.ErrorMessage{StatusCode: e.StatusCode, Error: e}
	}
	return &interfaces.ErrorMessage{StatusCode: e.StatusCode, Error: fmt.Errorf("%s", payload)}
}

// CheckRequest enforces API-key metadata policy for a request and returns the
// provider candidates that remain after policy filtering.
func CheckRequest(cfg *sdkconfig.SDKConfig, apiKey, handlerType string, providers []string, requestedModel, normalizedModel string, now time.Time) ([]string, *interfaces.ErrorMessage) {
	_ = handlerType
	apiKey = strings.TrimSpace(apiKey)
	if cfg == nil || apiKey == "" {
		return providers, nil
	}
	meta, ok := metadataForAPIKey(cfg, apiKey)
	if !ok {
		return providers, nil
	}

	status, reason := meta.EffectiveStatus(now)
	if status != internalconfig.APIKeyStatusActive {
		if reason == "" {
			reason = "API key is not active"
		}
		return nil, policyError{StatusCode: http.StatusForbidden, Code: errorCodePolicyViolation, Type: "permission_error", Message: reason}.errorMessage()
	}
	if err := validateQuotaPeriod(meta); err != nil {
		return nil, policyError{StatusCode: http.StatusForbidden, Code: errorCodePolicyViolation, Type: "permission_error", Message: err.Error()}.errorMessage()
	}

	if len(meta.AllowedModels) > 0 && !modelAllowed(meta.AllowedModels, modelCandidates(requestedModel, normalizedModel)) {
		model := strings.TrimSpace(requestedModel)
		if model == "" {
			model = strings.TrimSpace(normalizedModel)
		}
		if model == "" {
			model = "unknown"
		}
		return nil, policyError{
			StatusCode: http.StatusForbidden,
			Code:       errorCodePolicyViolation,
			Type:       "permission_error",
			Message:    fmt.Sprintf("API key is not allowed to use model %s", model),
		}.errorMessage()
	}

	if len(meta.AllowedProviders) > 0 {
		filtered := filterProviders(providers, meta.AllowedProviders)
		if len(filtered) == 0 {
			return nil, policyError{
				StatusCode: http.StatusForbidden,
				Code:       errorCodePolicyViolation,
				Type:       "permission_error",
				Message:    "API key is not allowed to use any provider for this model",
			}.errorMessage()
		}
		providers = filtered
	}

	statusQuota, err := StatusForAPIKey(apiKey, meta, now)
	if err != nil && QuotaConfigured(meta) {
		return nil, policyError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       errorCodeQuotaStoreUnavailable,
			Type:       "server_error",
			Message:    "quota store unavailable",
		}.errorMessage()
	}
	if statusQuota.Blocked {
		return nil, policyError{
			StatusCode: http.StatusForbidden,
			Code:       errorCodePolicyViolation,
			Type:       "permission_error",
			Message:    "API key quota exceeded",
		}.errorMessage()
	}

	return providers, nil
}

func metadataForAPIKey(cfg *sdkconfig.SDKConfig, apiKey string) (internalconfig.APIKeyMetadata, bool) {
	if cfg == nil {
		return internalconfig.APIKeyMetadata{}, false
	}
	found := false
	for _, configured := range cfg.APIKeys {
		if strings.TrimSpace(configured) == apiKey {
			found = true
			break
		}
	}
	if !found {
		return internalconfig.APIKeyMetadata{}, false
	}
	meta := internalconfig.NormalizeAPIKeyMetadata(cfg.APIKeyMetadata[internalconfig.APIKeyID(apiKey)])
	return meta, true
}

// ValidateMetadata validates operator-supplied API-key metadata before it is
// persisted by the management API.
func ValidateMetadata(meta internalconfig.APIKeyMetadata) error {
	if strings.TrimSpace(meta.ExpiresAt) != "" {
		if _, ok := meta.ExpiryTime(); !ok {
			return fmt.Errorf("expires-at is invalid")
		}
	}
	if err := validateQuotaPeriod(meta); err != nil {
		return err
	}
	if meta.TokenQuotaLimit < 0 {
		return fmt.Errorf("token-quota-limit cannot be negative")
	}
	if meta.USDQuotaLimit < 0 {
		return fmt.Errorf("usd-quota-limit cannot be negative")
	}
	if meta.DailyTokenLimit < 0 {
		return fmt.Errorf("daily-token-limit cannot be negative")
	}
	if meta.MonthlyBudgetUSD < 0 {
		return fmt.Errorf("monthly-budget-usd cannot be negative")
	}
	for _, provider := range meta.AllowedProviders {
		if strings.Contains(strings.TrimSpace(provider), "*") {
			return fmt.Errorf("allowed-providers does not support wildcards")
		}
	}
	for _, model := range meta.AllowedModels {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if strings.Count(model, "*") > 1 || (strings.Contains(model, "*") && !strings.HasSuffix(model, "*")) || model == "*" {
			return fmt.Errorf("allowed-models only supports suffix wildcards like gpt-5*")
		}
	}
	return nil
}

func validateQuotaPeriod(meta internalconfig.APIKeyMetadata) error {
	period := strings.ToLower(strings.TrimSpace(meta.QuotaPeriod))
	if period == "" {
		return nil
	}
	if period != internalconfig.APIKeyQuotaPeriodDaily && period != internalconfig.APIKeyQuotaPeriodOneTime {
		return fmt.Errorf("quota-period must be daily or one_time")
	}
	return nil
}

func filterProviders(providers []string, allowed []string) []string {
	if len(allowed) == 0 {
		return providers
	}
	if len(providers) == 0 {
		return nil
	}
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		for _, allowedProvider := range allowed {
			if strings.EqualFold(provider, strings.TrimSpace(allowedProvider)) {
				out = append(out, provider)
				break
			}
		}
	}
	return out
}

func modelAllowed(allowed []string, candidates []string) bool {
	for _, candidate := range candidates {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		for _, pattern := range allowed {
			pattern = strings.ToLower(strings.TrimSpace(pattern))
			if pattern == "" {
				continue
			}
			if strings.HasSuffix(pattern, "*") {
				prefix := strings.TrimSuffix(pattern, "*")
				if prefix != "" && strings.HasPrefix(candidate, prefix) {
					return true
				}
				continue
			}
			if candidate == pattern {
				return true
			}
		}
	}
	return false
}

func modelCandidates(requestedModel, normalizedModel string) []string {
	out := make([]string, 0, 8)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		out = append(out, value)
	}
	for _, model := range []string{requestedModel, normalizedModel} {
		add(model)
		noPrefix := stripProviderPrefix(model)
		add(noPrefix)
		noSuffix := stripThinkingSuffix(model)
		add(noSuffix)
		add(stripThinkingSuffix(noPrefix))
	}
	return out
}

func stripProviderPrefix(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[idx+1:])
	}
	return model
}

func stripThinkingSuffix(model string) string {
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	if !parsed.HasSuffix {
		return strings.TrimSpace(model)
	}
	return strings.TrimSpace(parsed.ModelName)
}
