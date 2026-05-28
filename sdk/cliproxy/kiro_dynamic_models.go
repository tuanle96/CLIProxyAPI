// Package cliproxy — Kiro dynamic model discovery.
//
// This file contains the integration glue that calls the Kiro
// ListAvailableModels API for each registered Kiro auth and merges the live
// model list with the static catalog (see internal/registry/kiro_models.go).
//
// The fetch runs asynchronously after the static catalog has been registered
// so that:
//   - The synchronous registration path is never blocked by network latency.
//   - When the API is unreachable the static catalog still surfaces in /v1/models.
//   - When the API responds with a different list (e.g. new preview models),
//     the registry is re-bound with the merged result.
package cliproxy

import (
	"context"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// kiroDynamicFetchTimeout caps each ListAvailableModels call to avoid stalling
// the background updater when the upstream is slow or unreachable.
const kiroDynamicFetchTimeout = 8 * time.Second

// refreshKiroDynamicModels schedules an asynchronous fetch of the live Kiro
// model list for the given auth and re-registers the merged catalog when the
// response is meaningfully different from the static set already on file.
//
// Callers must already have registered the static catalog (so /v1/models is
// not empty if the upstream call fails). The auth fields are snapshotted
// before the goroutine starts because the input pointer may be mutated by the
// auth manager once registerModelsForAuth returns.
func (s *Service) refreshKiroDynamicModels(a *coreauth.Auth, excluded []string) {
	if a == nil || a.ID == "" || a.Metadata == nil {
		return
	}
	accessToken := metaString(a.Metadata, "access_token")
	if strings.TrimSpace(accessToken) == "" {
		return
	}

	authID := a.ID
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	if provider == "" {
		provider = "kiro"
	}
	excludedCopy := append([]string(nil), excluded...)
	metadata := make(map[string]any, len(a.Metadata))
	for k, v := range a.Metadata {
		metadata[k] = v
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warnf("kiro: dynamic model fetch panicked for %s: %v", authID, r)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), kiroDynamicFetchTimeout)
		defer cancel()

		apiModels, err := s.fetchKiroAPIModels(ctx, metadata)
		if err != nil {
			log.Debugf("kiro: ListAvailableModels failed for %s: %v", authID, err)
			return
		}
		if len(apiModels) == 0 {
			return
		}
		converted := registry.ConvertKiroAPIModels(apiModels)
		if len(converted) == 0 {
			return
		}
		merged := registry.MergeWithStaticMetadata(converted, registry.GetKiroModels())
		merged = applyExcludedModels(merged, excludedCopy)
		if len(merged) == 0 {
			return
		}

		GlobalModelRegistry().RegisterClient(authID, provider, merged)
		log.Debugf("kiro: refreshed %d models from ListAvailableModels for %s", len(merged), authID)
	}()
}

// fetchKiroAPIModels invokes the Kiro ListAvailableModels REST endpoint using
// credentials extracted from the auth metadata snapshot. It returns the raw
// model entries converted into the registry-local KiroAPIModel struct so the
// caller can stay decoupled from the kiro auth package's wire types.
func (s *Service) fetchKiroAPIModels(ctx context.Context, metadata map[string]any) ([]*registry.KiroAPIModel, error) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil {
		return nil, nil
	}
	tokenData := &kiroauth.KiroTokenData{
		AccessToken:  metaString(metadata, "access_token"),
		RefreshToken: metaString(metadata, "refresh_token"),
		ProfileArn:   metaString(metadata, "profile_arn"),
		ClientID:     metaString(metadata, "client_id"),
		ClientSecret: metaString(metadata, "client_secret"),
		Region:       metaString(metadata, "region"),
		StartURL:     metaString(metadata, "start_url"),
		AuthMethod:   metaString(metadata, "auth_method"),
		Provider:     metaString(metadata, "provider"),
	}
	authClient := kiroauth.NewKiroAuth(cfg)

	apiModels, err := authClient.ListAvailableModels(ctx, tokenData)
	if err != nil {
		return nil, err
	}
	out := make([]*registry.KiroAPIModel, 0, len(apiModels))
	for _, m := range apiModels {
		if m == nil {
			continue
		}
		out = append(out, &registry.KiroAPIModel{
			ModelID:         m.ModelID,
			ModelName:       m.ModelName,
			Description:     m.Description,
			RateMultiplier:  m.RateMultiplier,
			RateUnit:        m.RateUnit,
			MaxInputTokens:  m.MaxInputTokens,
			MaxOutputTokens: m.MaxOutputTokens,
		})
	}
	return out, nil
}

// metaString reads a string field from the auth metadata, returning the empty
// string when the key is missing or holds a non-string value.
func metaString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
