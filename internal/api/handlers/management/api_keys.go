package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type apiKeyResponse struct {
	APIKeys  []string                         `json:"api-keys"`
	Items    []apiKeyResponseItem             `json:"items"`
	Metadata map[string]config.APIKeyMetadata `json:"metadata,omitempty"`
}

type apiKeyResponseItem struct {
	Index        int                      `json:"index"`
	ID           string                   `json:"id"`
	Key          string                   `json:"key"`
	MaskedKey    string                   `json:"masked-key"`
	Metadata     config.APIKeyMetadata    `json:"metadata"`
	Status       string                   `json:"status"`
	StatusReason string                   `json:"status-reason,omitempty"`
	QuotaStatus  apikeypolicy.QuotaStatus `json:"quota-status"`
}

type apiKeyWriteItem struct {
	Key      string                 `json:"key"`
	Metadata *config.APIKeyMetadata `json:"metadata"`
}

type apiKeyWriteBody struct {
	APIKeys []string          `json:"api-keys"`
	Items   []apiKeyWriteItem `json:"items"`
}

func (h *Handler) GetAPIKeys(c *gin.Context) {
	c.JSON(http.StatusOK, h.apiKeyResponse())
}

func (h *Handler) PutAPIKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	keys, metadata, err := h.parseAPIKeyPut(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.cfg.APIKeys = keys
	h.cfg.APIKeyMetadata = metadata
	h.cfg.SanitizeAPIKeyMetadata()
	h.persist(c)
}

func (h *Handler) PatchAPIKeys(c *gin.Context) {
	var body struct {
		Old      *string                `json:"old"`
		New      *string                `json:"new"`
		Index    *int                   `json:"index"`
		Value    *string                `json:"value"`
		Metadata *config.APIKeyMetadata `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	now := time.Now().UTC()
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.APIKeys) {
		index := *body.Index
		oldKey := h.cfg.APIKeys[index]
		if body.Value != nil {
			nextKey := strings.TrimSpace(*body.Value)
			if nextKey == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "api key cannot be empty"})
				return
			}
			h.cfg.APIKeys[index] = nextKey
			h.moveAPIKeyMetadata(oldKey, nextKey, now, "rotated")
		}
		if body.Metadata != nil {
			if err := apikeypolicy.ValidateMetadata(*body.Metadata); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			h.setAPIKeyMetadata(h.cfg.APIKeys[index], *body.Metadata, now, "updated")
		}
		h.cfg.SanitizeAPIKeyMetadata()
		h.persist(c)
		return
	}

	if body.Old != nil && body.New != nil {
		oldKey := strings.TrimSpace(*body.Old)
		newKey := strings.TrimSpace(*body.New)
		if newKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api key cannot be empty"})
			return
		}
		for i := range h.cfg.APIKeys {
			if h.cfg.APIKeys[i] == oldKey {
				h.cfg.APIKeys[i] = newKey
				h.moveAPIKeyMetadata(oldKey, newKey, now, "rotated")
				h.cfg.SanitizeAPIKeyMetadata()
				h.persist(c)
				return
			}
		}
		h.cfg.APIKeys = append(h.cfg.APIKeys, newKey)
		h.ensureAPIKeyMetadata(newKey, config.APIKeyMetadata{}, now, "created")
		h.cfg.SanitizeAPIKeyMetadata()
		h.persist(c)
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "missing fields"})
}

func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.APIKeys) {
			h.cfg.APIKeys = append(h.cfg.APIKeys[:idx], h.cfg.APIKeys[idx+1:]...)
			h.cfg.SanitizeAPIKeyMetadata()
			h.persist(c)
			return
		}
	}
	if val := strings.TrimSpace(c.Query("value")); val != "" {
		out := make([]string, 0, len(h.cfg.APIKeys))
		for _, key := range h.cfg.APIKeys {
			if strings.TrimSpace(key) != val {
				out = append(out, key)
			}
		}
		h.cfg.APIKeys = out
		h.cfg.SanitizeAPIKeyMetadata()
		h.persist(c)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "missing index or value"})
}

func (h *Handler) apiKeyResponse() apiKeyResponse {
	keys := append([]string(nil), h.cfg.APIKeys...)
	items := make([]apiKeyResponseItem, 0, len(keys))
	now := time.Now()
	for index, key := range keys {
		id := config.APIKeyID(key)
		meta := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[id])
		status, reason := meta.EffectiveStatus(now)
		quotaStatus, _ := apikeypolicy.StatusForAPIKey(key, meta, now)
		items = append(items, apiKeyResponseItem{
			Index:        index,
			ID:           id,
			Key:          key,
			MaskedKey:    config.MaskAPIKey(key),
			Metadata:     meta,
			Status:       status,
			StatusReason: reason,
			QuotaStatus:  quotaStatus,
		})
	}
	return apiKeyResponse{
		APIKeys:  keys,
		Items:    items,
		Metadata: cloneAPIKeyMetadata(h.cfg.APIKeyMetadata),
	}
}

func (h *Handler) parseAPIKeyPut(data []byte) ([]string, map[string]config.APIKeyMetadata, error) {
	var legacy []string
	if err := json.Unmarshal(data, &legacy); err == nil {
		keys := normalizeManagementAPIKeys(legacy)
		now := time.Now().UTC()
		metadata := make(map[string]config.APIKeyMetadata, len(keys))
		for _, key := range keys {
			id := config.APIKeyID(key)
			meta := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[id])
			if meta.CreatedAt == "" && !isZeroAPIKeyMetadata(meta) {
				meta.CreatedAt = now.Format(time.RFC3339)
			}
			if !isZeroAPIKeyMetadata(meta) {
				metadata[id] = meta
			}
		}
		return keys, metadataOrNil(metadata), nil
	}

	var body apiKeyWriteBody
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, nil, fmt.Errorf("invalid body")
	}
	items := body.Items
	if len(items) == 0 && len(body.APIKeys) > 0 {
		keys := normalizeManagementAPIKeys(body.APIKeys)
		now := time.Now().UTC()
		metadata := make(map[string]config.APIKeyMetadata, len(keys))
		for _, key := range keys {
			id := config.APIKeyID(key)
			meta := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[id])
			if !isZeroAPIKeyMetadata(meta) {
				metadata[id] = meta
			} else {
				metadata[id] = appendAPIKeyAudit(config.APIKeyMetadata{
					CreatedAt: now.Format(time.RFC3339),
					UpdatedAt: now.Format(time.RFC3339),
				}, "created", now, "API key added")
			}
		}
		return keys, metadataOrNil(metadata), nil
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("missing api key items")
	}

	now := time.Now().UTC()
	keys := make([]string, 0, len(items))
	metadata := make(map[string]config.APIKeyMetadata, len(items))
	for index, item := range items {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			return nil, nil, fmt.Errorf("items[%d].key is required", index)
		}
		keys = append(keys, key)

		oldKey := ""
		if index < len(h.cfg.APIKeys) {
			oldKey = strings.TrimSpace(h.cfg.APIKeys[index])
		}
		meta := config.APIKeyMetadata{}
		oldID := config.APIKeyID(oldKey)
		newID := config.APIKeyID(key)
		base := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[newID])
		if isZeroAPIKeyMetadata(base) && oldID != "" {
			base = config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[oldID])
		}
		if item.Metadata != nil {
			if err := apikeypolicy.ValidateMetadata(*item.Metadata); err != nil {
				return nil, nil, fmt.Errorf("items[%d].metadata: %w", index, err)
			}
			meta = config.NormalizeAPIKeyMetadata(*item.Metadata)
		} else {
			meta = base
		}
		if meta.CreatedAt == "" {
			if base.CreatedAt != "" {
				meta.CreatedAt = base.CreatedAt
			} else {
				meta.CreatedAt = now.Format(time.RFC3339)
			}
		}
		meta = applyAPIKeyLifecycleTimestamps(meta, base, now)
		changed := !reflect.DeepEqual(meta, base)
		switch {
		case oldKey == "":
			meta.UpdatedAt = now.Format(time.RFC3339)
			meta = appendAPIKeyAudit(meta, "created", now, "API key added")
		case oldKey != key:
			meta.UpdatedAt = now.Format(time.RFC3339)
			meta.LastRotatedAt = now.Format(time.RFC3339)
			meta = appendAPIKeyAudit(meta, "rotated", now, "API key rotated")
		case changed:
			meta.UpdatedAt = now.Format(time.RFC3339)
			meta = appendAPIKeyAudit(meta, "updated", now, "API key metadata updated")
		}
		metadata[newID] = config.NormalizeAPIKeyMetadata(meta)
	}

	return keys, metadataOrNil(metadata), nil
}

func (h *Handler) moveAPIKeyMetadata(oldKey, newKey string, now time.Time, eventType string) {
	oldID := config.APIKeyID(oldKey)
	newID := config.APIKeyID(newKey)
	if newID == "" {
		return
	}
	meta := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[oldID])
	if meta.CreatedAt == "" {
		meta.CreatedAt = now.Format(time.RFC3339)
	}
	meta.UpdatedAt = now.Format(time.RFC3339)
	if oldID != "" && oldID != newID {
		meta.LastRotatedAt = now.Format(time.RFC3339)
		delete(h.cfg.APIKeyMetadata, oldID)
	}
	meta = appendAPIKeyAudit(meta, eventType, now, "API key "+eventType)
	if h.cfg.APIKeyMetadata == nil {
		h.cfg.APIKeyMetadata = make(map[string]config.APIKeyMetadata)
	}
	h.cfg.APIKeyMetadata[newID] = meta
}

func (h *Handler) setAPIKeyMetadata(key string, meta config.APIKeyMetadata, now time.Time, eventType string) {
	id := config.APIKeyID(key)
	if id == "" {
		return
	}
	if h.cfg.APIKeyMetadata == nil {
		h.cfg.APIKeyMetadata = make(map[string]config.APIKeyMetadata)
	}
	current := config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[id])
	next := config.NormalizeAPIKeyMetadata(meta)
	if next.CreatedAt == "" {
		if current.CreatedAt != "" {
			next.CreatedAt = current.CreatedAt
		} else {
			next.CreatedAt = now.Format(time.RFC3339)
		}
	}
	next = applyAPIKeyLifecycleTimestamps(next, current, now)
	next.UpdatedAt = now.Format(time.RFC3339)
	h.cfg.APIKeyMetadata[id] = appendAPIKeyAudit(next, eventType, now, "API key "+eventType)
}

func (h *Handler) ensureAPIKeyMetadata(key string, meta config.APIKeyMetadata, now time.Time, eventType string) {
	if h.cfg.APIKeyMetadata == nil {
		h.cfg.APIKeyMetadata = make(map[string]config.APIKeyMetadata)
	}
	id := config.APIKeyID(key)
	if id == "" {
		return
	}
	if existing, ok := h.cfg.APIKeyMetadata[id]; ok {
		h.cfg.APIKeyMetadata[id] = config.NormalizeAPIKeyMetadata(existing)
		return
	}
	if meta.CreatedAt == "" {
		meta.CreatedAt = now.Format(time.RFC3339)
	}
	if meta.UpdatedAt == "" {
		meta.UpdatedAt = now.Format(time.RFC3339)
	}
	h.cfg.APIKeyMetadata[id] = appendAPIKeyAudit(config.NormalizeAPIKeyMetadata(meta), eventType, now, "API key "+eventType)
}

func normalizeManagementAPIKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func appendAPIKeyAudit(meta config.APIKeyMetadata, eventType string, at time.Time, message string) config.APIKeyMetadata {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return meta
	}
	meta.Audit = append(meta.Audit, config.APIKeyAuditEvent{
		Type:    eventType,
		At:      at.UTC().Format(time.RFC3339),
		Message: strings.TrimSpace(message),
	})
	const maxEvents = 20
	if len(meta.Audit) > maxEvents {
		meta.Audit = meta.Audit[len(meta.Audit)-maxEvents:]
	}
	return meta
}

func applyAPIKeyLifecycleTimestamps(meta, previous config.APIKeyMetadata, now time.Time) config.APIKeyMetadata {
	if meta.Disabled && meta.DisabledAt == "" {
		if previous.Disabled && previous.DisabledAt != "" {
			meta.DisabledAt = previous.DisabledAt
		} else {
			meta.DisabledAt = now.UTC().Format(time.RFC3339)
		}
	}
	if meta.Revoked && meta.RevokedAt == "" {
		if previous.Revoked && previous.RevokedAt != "" {
			meta.RevokedAt = previous.RevokedAt
		} else {
			meta.RevokedAt = now.UTC().Format(time.RFC3339)
		}
	}
	return meta
}

func cloneAPIKeyMetadata(metadata map[string]config.APIKeyMetadata) map[string]config.APIKeyMetadata {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]config.APIKeyMetadata, len(metadata))
	for id, meta := range metadata {
		out[id] = meta
	}
	return out
}

func metadataOrNil(metadata map[string]config.APIKeyMetadata) map[string]config.APIKeyMetadata {
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func isZeroAPIKeyMetadata(meta config.APIKeyMetadata) bool {
	var zero config.APIKeyMetadata
	return reflect.DeepEqual(meta, zero) || bytes.Equal(mustMarshalAPIKeyMetadata(meta), mustMarshalAPIKeyMetadata(zero))
}

func mustMarshalAPIKeyMetadata(meta config.APIKeyMetadata) []byte {
	data, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return data
}
