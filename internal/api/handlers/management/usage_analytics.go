package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeypolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usageportal"
)

var usageAnalyticsPeriods = map[string]struct{}{
	"today": {},
	"24h":   {},
	"7d":    {},
	"30d":   {},
	"60d":   {},
	"all":   {},
}

var usageAnalyticsChartPeriods = map[string]struct{}{
	"today": {},
	"24h":   {},
	"7d":    {},
	"30d":   {},
	"60d":   {},
}

func (h *Handler) GetUsageAnalyticsStats(c *gin.Context) {
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsPeriods, "today")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	snapshot := usageportal.Analytics(period, time.Now())
	h.decorateUsageAnalyticsSnapshot(&snapshot)
	c.JSON(http.StatusOK, snapshot)
}

func (h *Handler) GetUsageAnalyticsChart(c *gin.Context) {
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsChartPeriods, "7d")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	snapshot := usageportal.Analytics(period, time.Now())
	c.JSON(http.StatusOK, snapshot.Series)
}

func (h *Handler) GetUsageAnalyticsRequestDetails(c *gin.Context) {
	filter, err := h.parseUsageAnalyticsRequestDetailsFilter(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	snapshot := usageportal.RequestDetails(filter, time.Now())
	h.decorateUsageAnalyticsRequestDetails(&snapshot)
	c.JSON(http.StatusOK, snapshot)
}

func (h *Handler) GetUsageAnalyticsProviders(c *gin.Context) {
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsPeriods, "all")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	snapshot := usageportal.Analytics(period, time.Now())
	h.decorateUsageAnalyticsSnapshot(&snapshot)
	providers := make([]gin.H, 0, len(snapshot.ByProvider))
	for _, group := range snapshot.ByProvider {
		providers = append(providers, gin.H{
			"id":            group.Provider,
			"name":          group.Provider,
			"requests":      group.Requests,
			"success":       group.Success,
			"failed":        group.Failed,
			"input_tokens":  group.InputTokens,
			"output_tokens": group.OutputTokens,
			"total_tokens":  group.TotalTokens,
			"cost_usd":      group.CostUSD,
			"last_used":     group.LastUsed,
		})
	}
	c.JSON(http.StatusOK, gin.H{"period": snapshot.Period, "providers": providers})
}

func (h *Handler) GetUsageAnalyticsProviderBreakdown(c *gin.Context) {
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsPeriods, "today")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	providerFilter := strings.TrimSpace(c.Query("provider"))
	snapshot := usageportal.Analytics(period, time.Now())
	h.decorateUsageAnalyticsSnapshot(&snapshot)

	providers := make([]gin.H, 0, len(snapshot.ByProvider))
	totals := gin.H{
		"total_providers":         int64(0),
		"total_accounts":          int64(0),
		"total_requests":          int64(0),
		"total_prompt_tokens":     int64(0),
		"total_completion_tokens": int64(0),
		"total_tokens":            int64(0),
		"total_cost_usd":          float64(0),
	}

	for _, provider := range snapshot.ByProvider {
		if providerFilter != "" && !strings.EqualFold(provider.Provider, providerFilter) {
			continue
		}
		accounts := usageAnalyticsAccountsForProvider(snapshot.ByAccount, provider.Provider)
		providerPayload := gin.H{
			"id":                provider.Provider,
			"requests":          provider.Requests,
			"prompt_tokens":     provider.InputTokens,
			"completion_tokens": provider.OutputTokens,
			"total_tokens":      provider.TotalTokens,
			"cost_usd":          provider.CostUSD,
			"last_used":         provider.LastUsed,
			"success_requests":  provider.Success,
			"error_requests":    provider.Failed,
			"account_count":     len(accounts),
			"accounts":          accounts,
		}
		providers = append(providers, providerPayload)

		totals["total_providers"] = totals["total_providers"].(int64) + 1
		totals["total_accounts"] = totals["total_accounts"].(int64) + int64(len(accounts))
		totals["total_requests"] = totals["total_requests"].(int64) + provider.Requests
		totals["total_prompt_tokens"] = totals["total_prompt_tokens"].(int64) + provider.InputTokens
		totals["total_completion_tokens"] = totals["total_completion_tokens"].(int64) + provider.OutputTokens
		totals["total_tokens"] = totals["total_tokens"].(int64) + provider.TotalTokens
		totals["total_cost_usd"] = totals["total_cost_usd"].(float64) + provider.CostUSD
	}

	c.JSON(http.StatusOK, gin.H{
		"period":       snapshot.Period,
		"generated_at": time.Now().UTC(),
		"totals":       totals,
		"providers":    providers,
	})
}

func (h *Handler) GetUsageAnalyticsAPIKey(c *gin.Context) {
	apiKey, found := h.resolveUsageAnalyticsAPIKey(c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsChartPeriods, "today")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	windowDays := usageAnalyticsWindowDays(period)
	snapshot := usageportal.SnapshotForKey(apiKey, windowDays, true, time.Now())
	h.decorateUsageAnalyticsKeySnapshot(&snapshot, apiKey)
	identity := h.usageAnalyticsAPIKeyIndex().identityForAPIKey(apiKey)
	meta := config.APIKeyMetadata{}
	if h.cfg != nil {
		meta = config.NormalizeAPIKeyMetadata(h.cfg.APIKeyMetadata[config.APIKeyID(apiKey)])
	}
	quotaStatus, _ := apikeypolicy.StatusForAPIKey(apiKey, meta, time.Now())
	c.JSON(http.StatusOK, gin.H{
		"key": gin.H{
			"id":                  c.Param("id"),
			"name":                snapshot.KeyLabel,
			"api_key_name":        identity.Name,
			"api_key_fingerprint": identity.Fingerprint,
			"display_label":       identity.DisplayLabel,
			"active":              true,
		},
		"period":   period,
		"stats":    snapshot.Totals,
		"chart":    snapshot.Series,
		"requests": snapshot.RecentRequests,
		"quotas":   []apikeypolicy.QuotaStatus{quotaStatus},
	})
}

func (h *Handler) StreamUsageAnalytics(c *gin.Context) {
	period, ok := parseUsageAnalyticsPeriod(c.Query("period"), usageAnalyticsPeriods, "today")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	sendSnapshot := func() bool {
		snapshot := usageportal.Analytics(period, time.Now())
		h.decorateUsageAnalyticsSnapshot(&snapshot)
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return false
		}
		if _, err = fmt.Fprintf(c.Writer, "data: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !sendSnapshot() {
		return
	}

	updates := usageportal.Subscribe(c.Request.Context())
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			if !sendSnapshot() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(c.Writer, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func usageAnalyticsAccountsForProvider(accounts []usageportal.AnalyticsGroup, provider string) []gin.H {
	out := make([]gin.H, 0)
	for _, account := range accounts {
		if provider != "" && !strings.EqualFold(account.Provider, provider) {
			continue
		}
		out = append(out, gin.H{
			"id":                account.Key,
			"connection_id":     "",
			"provider":          account.Provider,
			"label":             account.AccountLabel,
			"secondary":         "",
			"requests":          account.Requests,
			"prompt_tokens":     account.InputTokens,
			"completion_tokens": account.OutputTokens,
			"total_tokens":      account.TotalTokens,
			"cost_usd":          account.CostUSD,
			"last_used":         account.LastUsed,
			"success_requests":  account.Success,
			"error_requests":    account.Failed,
		})
	}
	return out
}

func (h *Handler) parseUsageAnalyticsRequestDetailsFilter(c *gin.Context) (usageportal.RequestDetailsFilter, error) {
	page, err := parsePositiveQueryInt(c, "page", 1, 0)
	if err != nil {
		return usageportal.RequestDetailsFilter{}, err
	}
	pageSize, err := parsePositiveQueryInt(c, "page_size", 20, 100)
	if err != nil {
		return usageportal.RequestDetailsFilter{}, err
	}
	if raw := strings.TrimSpace(c.Query("pageSize")); raw != "" {
		pageSize, err = parsePositiveInt(raw, "pageSize", 100)
		if err != nil {
			return usageportal.RequestDetailsFilter{}, err
		}
	}
	start, err := parseUsageAnalyticsTime(firstNonEmpty(c.Query("start"), c.Query("startDate")), "start")
	if err != nil {
		return usageportal.RequestDetailsFilter{}, err
	}
	end, err := parseUsageAnalyticsTime(firstNonEmpty(c.Query("end"), c.Query("endDate")), "end")
	if err != nil {
		return usageportal.RequestDetailsFilter{}, err
	}
	return usageportal.RequestDetailsFilter{
		Page:      page,
		PageSize:  pageSize,
		Provider:  c.Query("provider"),
		Model:     c.Query("model"),
		APIKey:    h.resolveUsageAnalyticsAPIKeyFilter(firstNonEmpty(c.Query("api_key"), c.Query("apiKey"))),
		Endpoint:  c.Query("endpoint"),
		Status:    c.Query("status"),
		StartTime: start,
		EndTime:   end,
	}, nil
}

type usageAnalyticsAPIKeyIdentity struct {
	Name         string
	Fingerprint  string
	DisplayLabel string
}

type usageAnalyticsAPIKeyIndex struct {
	byFingerprint map[string]usageAnalyticsAPIKeyIdentity
	byName        map[string][]usageAnalyticsAPIKeyIdentity
}

func (h *Handler) usageAnalyticsAPIKeyIndex() usageAnalyticsAPIKeyIndex {
	index := usageAnalyticsAPIKeyIndex{
		byFingerprint: make(map[string]usageAnalyticsAPIKeyIdentity),
		byName:        make(map[string][]usageAnalyticsAPIKeyIdentity),
	}
	if h == nil {
		return index
	}
	h.mu.Lock()
	cfg := h.cfg
	keys := []string(nil)
	metadata := map[string]config.APIKeyMetadata(nil)
	if cfg != nil {
		keys = append(keys, cfg.APIKeys...)
		if len(cfg.APIKeyMetadata) > 0 {
			metadata = make(map[string]config.APIKeyMetadata, len(cfg.APIKeyMetadata))
			for key, value := range cfg.APIKeyMetadata {
				metadata[key] = value
			}
		}
	}
	h.mu.Unlock()
	if cfg == nil {
		return index
	}
	for _, apiKey := range keys {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		identity := index.identityForConfiguredAPIKey(apiKey, metadata)
		if identity.Fingerprint == "" {
			continue
		}
		index.byFingerprint[strings.ToLower(identity.Fingerprint)] = identity
		if identity.Name != "" {
			nameKey := strings.ToLower(identity.Name)
			index.byName[nameKey] = append(index.byName[nameKey], identity)
		}
	}
	return index
}

func (idx usageAnalyticsAPIKeyIndex) identityForConfiguredAPIKey(apiKey string, metadata map[string]config.APIKeyMetadata) usageAnalyticsAPIKeyIdentity {
	fingerprint := config.MaskAPIKey(apiKey)
	meta := config.NormalizeAPIKeyMetadata(metadata[config.APIKeyID(apiKey)])
	displayLabel := fingerprint
	if meta.Name != "" {
		displayLabel = meta.Name
	}
	return usageAnalyticsAPIKeyIdentity{
		Name:         meta.Name,
		Fingerprint:  fingerprint,
		DisplayLabel: displayLabel,
	}
}

func (idx usageAnalyticsAPIKeyIndex) identityForAPIKey(apiKey string) usageAnalyticsAPIKeyIdentity {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return usageAnalyticsAPIKeyIdentity{}
	}
	fingerprint := config.MaskAPIKey(apiKey)
	if identity, ok := idx.byFingerprint[strings.ToLower(fingerprint)]; ok {
		return identity
	}
	return usageAnalyticsAPIKeyIdentity{
		Fingerprint:  fingerprint,
		DisplayLabel: fingerprint,
	}
}

func (idx usageAnalyticsAPIKeyIndex) identityForLabel(label string) usageAnalyticsAPIKeyIdentity {
	label = strings.TrimSpace(label)
	if label == "" {
		return usageAnalyticsAPIKeyIdentity{}
	}
	if identity, ok := idx.byFingerprint[strings.ToLower(label)]; ok {
		return identity
	}
	return usageAnalyticsAPIKeyIdentity{
		Fingerprint:  label,
		DisplayLabel: label,
	}
}

func (idx usageAnalyticsAPIKeyIndex) resolveFilter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if identity, ok := idx.byFingerprint[strings.ToLower(value)]; ok {
		return identity.Fingerprint
	}
	matches := idx.byName[strings.ToLower(value)]
	if len(matches) == 1 {
		return matches[0].Fingerprint
	}
	return value
}

func (h *Handler) resolveUsageAnalyticsAPIKeyFilter(value string) string {
	return h.usageAnalyticsAPIKeyIndex().resolveFilter(value)
}

func (h *Handler) decorateUsageAnalyticsSnapshot(snapshot *usageportal.AnalyticsSnapshot) {
	if snapshot == nil {
		return
	}
	index := h.usageAnalyticsAPIKeyIndex()
	for i := range snapshot.ByAPIKey {
		applyUsageAnalyticsIdentityToGroup(&snapshot.ByAPIKey[i], index.identityForLabel(snapshot.ByAPIKey[i].APIKeyLabel))
	}
	for i := range snapshot.RecentRequests {
		applyUsageAnalyticsIdentityToRecentRequest(&snapshot.RecentRequests[i], index.identityForLabel(snapshot.RecentRequests[i].APIKeyLabel))
	}
	for i := range snapshot.ActiveRequests {
		applyUsageAnalyticsIdentityToActiveRequest(&snapshot.ActiveRequests[i], index.identityForLabel(snapshot.ActiveRequests[i].APIKeyLabel))
	}
}

func (h *Handler) decorateUsageAnalyticsKeySnapshot(snapshot *usageportal.Snapshot, apiKey string) {
	if snapshot == nil {
		return
	}
	index := h.usageAnalyticsAPIKeyIndex()
	identity := index.identityForAPIKey(apiKey)
	if identity.Fingerprint != "" {
		snapshot.KeyLabel = identity.DisplayLabel
	}
	for i := range snapshot.RecentRequests {
		applyUsageAnalyticsIdentityToRecentRequest(&snapshot.RecentRequests[i], index.identityForLabel(snapshot.RecentRequests[i].APIKeyLabel))
	}
}

func (h *Handler) decorateUsageAnalyticsRequestDetails(snapshot *usageportal.RequestDetailsSnapshot) {
	if snapshot == nil {
		return
	}
	index := h.usageAnalyticsAPIKeyIndex()
	for i := range snapshot.Details {
		applyUsageAnalyticsIdentityToRequestDetail(&snapshot.Details[i], index.identityForLabel(snapshot.Details[i].APIKeyLabel))
	}
}

func applyUsageAnalyticsIdentityToGroup(group *usageportal.AnalyticsGroup, identity usageAnalyticsAPIKeyIdentity) {
	if group == nil || identity.Fingerprint == "" {
		return
	}
	group.APIKeyName = identity.Name
	group.APIKeyFingerprint = identity.Fingerprint
	group.APIKeyDisplayLabel = identity.DisplayLabel
}

func applyUsageAnalyticsIdentityToRecentRequest(request *usageportal.RecentRequest, identity usageAnalyticsAPIKeyIdentity) {
	if request == nil || identity.Fingerprint == "" {
		return
	}
	request.APIKeyName = identity.Name
	request.APIKeyFingerprint = identity.Fingerprint
	request.APIKeyDisplayLabel = identity.DisplayLabel
}

func applyUsageAnalyticsIdentityToActiveRequest(request *usageportal.ActiveRequest, identity usageAnalyticsAPIKeyIdentity) {
	if request == nil || identity.Fingerprint == "" {
		return
	}
	request.APIKeyName = identity.Name
	request.APIKeyFingerprint = identity.Fingerprint
	request.APIKeyDisplayLabel = identity.DisplayLabel
}

func applyUsageAnalyticsIdentityToRequestDetail(detail *usageportal.RequestDetail, identity usageAnalyticsAPIKeyIdentity) {
	if detail == nil || identity.Fingerprint == "" {
		return
	}
	detail.APIKeyName = identity.Name
	detail.APIKeyFingerprint = identity.Fingerprint
	detail.APIKeyDisplayLabel = identity.DisplayLabel
}

func parseUsageAnalyticsPeriod(value string, valid map[string]struct{}, fallback string) (string, bool) {
	period := strings.ToLower(strings.TrimSpace(value))
	if period == "" {
		period = fallback
	}
	period = normalizeUsageAnalyticsAPIKeyPeriod(period)
	_, ok := valid[period]
	return period, ok
}

func normalizeUsageAnalyticsAPIKeyPeriod(period string) string {
	period = strings.ToLower(strings.TrimSpace(period))
	switch period {
	case "7day":
		return "7d"
	case "30day":
		return "30d"
	case "":
		return "today"
	default:
		return period
	}
}

func usageAnalyticsWindowDays(period string) int {
	switch normalizeUsageAnalyticsAPIKeyPeriod(period) {
	case "today", "24h":
		return 1
	case "30d":
		return 30
	case "60d":
		return 60
	default:
		return 7
	}
}

func parsePositiveQueryInt(c *gin.Context, key string, fallback int, max int) (int, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback, nil
	}
	return parsePositiveInt(raw, key, max)
}

func parsePositiveInt(raw string, name string, max int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	if max > 0 && value > max {
		return 0, fmt.Errorf("%s must be <= %d", name, max)
	}
	return value, nil
}

func parseUsageAnalyticsTime(value string, name string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
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
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid %s time", name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) resolveUsageAnalyticsAPIKey(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" || h == nil {
		return "", false
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg == nil {
		return "", false
	}
	if index, err := strconv.Atoi(id); err == nil && index >= 0 && index < len(cfg.APIKeys) {
		return strings.TrimSpace(cfg.APIKeys[index]), strings.TrimSpace(cfg.APIKeys[index]) != ""
	}
	for _, apiKey := range cfg.APIKeys {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		if apiKey == id || usageportal.MaskAPIKey(apiKey) == id {
			return apiKey, true
		}
	}
	return "", false
}
