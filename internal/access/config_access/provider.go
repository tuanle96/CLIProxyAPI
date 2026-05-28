package configaccess

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	if len(keys) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keys, cfg.APIKeyMetadata),
	)
}

type provider struct {
	name string
	keys map[string]internalconfig.APIKeyMetadata
}

func newProvider(name string, keys []string, metadata map[string]internalconfig.APIKeyMetadata) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string]internalconfig.APIKeyMetadata, len(keys))
	for _, key := range keys {
		meta := internalconfig.NormalizeAPIKeyMetadata(metadata[internalconfig.APIKeyID(key)])
		keySet[key] = meta
	}
	return &provider{name: providerName, keys: keySet}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if meta, ok := p.keys[candidate.value]; ok {
			status, reason := meta.EffectiveStatus(time.Now())
			if status != internalconfig.APIKeyStatusActive {
				return nil, sdkaccess.NewForbiddenCredentialError(reason)
			}
			if len(meta.IPAllowlist) > 0 && !requestIPAllowed(r, meta.IPAllowlist) {
				return nil, sdkaccess.NewForbiddenCredentialError("API key is not allowed from this IP")
			}
			keyID := internalconfig.APIKeyID(candidate.value)
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata: map[string]string{
					"source":      candidate.source,
					"api_key_id":  keyID,
					"name":        meta.Name,
					"owner":       meta.Owner,
					"environment": meta.Environment,
				},
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func requestIPAllowed(r *http.Request, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	addr := ""
	if r != nil {
		addr = strings.TrimSpace(r.RemoteAddr)
	}
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		addr = host
	}
	ip, err := netip.ParseAddr(strings.Trim(addr, "[]"))
	if err != nil {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil && prefix.Contains(ip) {
			return true
		}
		if allowedIP, err := netip.ParseAddr(entry); err == nil && allowedIP == ip {
			return true
		}
	}
	return false
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
