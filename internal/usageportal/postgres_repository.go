package usageportal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultUsageEventsTable        = "usage_events"
	defaultUsageRollupsHourlyTable = "usage_rollups_hourly"
	defaultUsageRollupsDailyTable  = "usage_rollups_daily"
	defaultRollupQueryMinEvents    = int64(50000)
)

type PostgresRepositoryConfig struct {
	DSN                  string
	Schema               string
	EventsTable          string
	HourlyRollupTable    string
	DailyRollupTable     string
	RollupsEnabled       bool
	RollupQueryMinEvents int64
}

type PostgresRepository struct {
	db  *sql.DB
	cfg PostgresRepositoryConfig
}

func NewPostgresRepository(ctx context.Context, cfg PostgresRepositoryConfig) (*PostgresRepository, error) {
	cfg.DSN = strings.TrimSpace(cfg.DSN)
	if cfg.DSN == "" {
		return nil, fmt.Errorf("usage postgres repository: DSN is required")
	}
	if cfg.EventsTable == "" {
		cfg.EventsTable = defaultUsageEventsTable
	}
	if cfg.HourlyRollupTable == "" {
		cfg.HourlyRollupTable = defaultUsageRollupsHourlyTable
	}
	if cfg.DailyRollupTable == "" {
		cfg.DailyRollupTable = defaultUsageRollupsDailyTable
	}
	if cfg.RollupQueryMinEvents < 0 {
		cfg.RollupQueryMinEvents = 0
	}
	if cfg.RollupQueryMinEvents == 0 && cfg.RollupsEnabled {
		cfg.RollupQueryMinEvents = defaultRollupQueryMinEvents
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("usage postgres repository: open database connection: %w", err)
	}
	repo := &PostgresRepository{db: db, cfg: cfg}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage postgres repository: ping database: %w", err)
	}
	if err = repo.EnsureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func (r *PostgresRepository) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *PostgresRepository) EnsureSchema(ctx context.Context) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("usage postgres repository: not initialized")
	}
	if schema := strings.TrimSpace(r.cfg.Schema); schema != "" {
		if _, err := r.db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteSQLIdentifier(schema))); err != nil {
			return fmt.Errorf("usage postgres repository: create schema: %w", err)
		}
	}
	eventsTable := r.fullTableName(r.cfg.EventsTable)
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL DEFAULT '',
			timestamp TIMESTAMPTZ NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			alias TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			account_label TEXT NOT NULL DEFAULT '',
			api_key_label TEXT NOT NULL DEFAULT '',
			api_key_hash TEXT NOT NULL DEFAULT '',
			auth_index TEXT NOT NULL DEFAULT '',
			auth_type TEXT NOT NULL DEFAULT '',
			endpoint TEXT NOT NULL DEFAULT '',
			reasoning_effort TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL DEFAULT 0,
			failed BOOLEAN NOT NULL DEFAULT FALSE,
			latency_ms BIGINT NOT NULL DEFAULT 0,
			ttft_ms BIGINT NOT NULL DEFAULT 0,
			input_tokens BIGINT NOT NULL DEFAULT 0,
			output_tokens BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0,
			cached_tokens BIGINT NOT NULL DEFAULT 0,
			cache_read_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			request_json JSONB,
			provider_request_json JSONB,
			provider_response_json JSONB,
			response_json JSONB,
			websocket_timeline_json JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, eventsTable)); err != nil {
		return fmt.Errorf("usage postgres repository: create events table: %w", err)
	}
	indexes := []string{
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (timestamp DESC)", quoteSQLIdentifier(r.indexName("usage_events_timestamp_idx")), eventsTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (api_key_hash, timestamp DESC)", quoteSQLIdentifier(r.indexName("usage_events_api_key_time_idx")), eventsTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (provider, timestamp DESC)", quoteSQLIdentifier(r.indexName("usage_events_provider_time_idx")), eventsTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (model, timestamp DESC)", quoteSQLIdentifier(r.indexName("usage_events_model_time_idx")), eventsTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (endpoint, timestamp DESC)", quoteSQLIdentifier(r.indexName("usage_events_endpoint_time_idx")), eventsTable),
	}
	for _, query := range indexes {
		if _, err := r.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("usage postgres repository: create event index: %w", err)
		}
	}
	if err := r.ensureRollupTable(ctx, r.cfg.HourlyRollupTable); err != nil {
		return err
	}
	if err := r.ensureRollupTable(ctx, r.cfg.DailyRollupTable); err != nil {
		return err
	}
	return nil
}

func (r *PostgresRepository) ensureRollupTable(ctx context.Context, table string) error {
	tableName := r.fullTableName(table)
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			bucket_start TIMESTAMPTZ NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			alias TEXT NOT NULL DEFAULT '',
			account_label TEXT NOT NULL DEFAULT '',
			api_key_label TEXT NOT NULL DEFAULT '',
			endpoint TEXT NOT NULL DEFAULT '',
			requests BIGINT NOT NULL DEFAULT 0,
			success BIGINT NOT NULL DEFAULT 0,
			failed BIGINT NOT NULL DEFAULT 0,
			input_tokens BIGINT NOT NULL DEFAULT 0,
			output_tokens BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0,
			cached_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			last_used TIMESTAMPTZ,
			PRIMARY KEY (bucket_start, provider, model, alias, account_label, api_key_label, endpoint)
		)
	`, tableName)); err != nil {
		return fmt.Errorf("usage postgres repository: create rollup table: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON %s (bucket_start DESC)",
		quoteSQLIdentifier(r.indexName(table+"_bucket_idx")),
		tableName,
	)); err != nil {
		return fmt.Errorf("usage postgres repository: create rollup index: %w", err)
	}
	return nil
}

func (r *PostgresRepository) InsertEvent(ctx context.Context, event UsageEvent) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("usage postgres repository: not initialized")
	}
	if strings.TrimSpace(event.Detail.ID) == "" {
		return nil
	}
	if event.Detail.Timestamp.IsZero() {
		event.Detail.Timestamp = event.Request.Time
	}
	if event.Detail.Timestamp.IsZero() {
		event.Detail.Timestamp = time.Now()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage postgres repository: begin insert event: %w", err)
	}
	inserted, err := r.insertEventTx(ctx, tx, event)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if inserted {
		if err = r.upsertRollupTx(ctx, tx, r.cfg.HourlyRollupTable, event.Detail.Timestamp.UTC().Truncate(time.Hour), event); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = r.upsertRollupTx(ctx, tx, r.cfg.DailyRollupTable, startOfLocalDay(event.Detail.Timestamp).UTC(), event); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage postgres repository: commit insert event: %w", err)
	}
	return nil
}

func (r *PostgresRepository) insertEventTx(ctx context.Context, tx *sql.Tx, event UsageEvent) (bool, error) {
	detail := event.Detail
	timestamp := detail.Timestamp
	if timestamp.IsZero() {
		timestamp = event.Request.Time
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	tokens := detail.Tokens
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (
			id, request_id, timestamp, provider, model, alias, source, account_label, api_key_label,
			api_key_hash, auth_index, auth_type, endpoint, reasoning_effort, status, status_code, failed,
			latency_ms, ttft_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			cache_read_tokens, cache_creation_tokens, total_tokens, cost_usd, request_json,
			provider_request_json, provider_response_json, response_json, websocket_timeline_json
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
			$17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32
		)
		ON CONFLICT (id) DO NOTHING
	`, r.fullTableName(r.cfg.EventsTable)),
		detail.ID,
		strings.TrimSpace(detail.RequestID),
		timestamp.UTC(),
		detail.Provider,
		detail.Model,
		detail.Alias,
		event.Request.Source,
		detail.AccountLabel,
		detail.APIKeyLabel,
		event.APIKeyHash,
		event.Request.AuthIndex,
		event.Request.AuthType,
		detail.Endpoint,
		detail.ReasoningEffort,
		detail.Status,
		detail.StatusCode,
		detail.Failed,
		detail.Latency.TotalMs,
		detail.Latency.TTFTMs,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.CacheReadTokens,
		tokens.CacheCreationTokens,
		tokens.TotalTokens,
		detail.CostUSD,
		jsonbValue(detail.Request),
		jsonbValue(detail.ProviderRequest),
		jsonbValue(detail.ProviderResponse),
		jsonbValue(detail.Response),
		jsonbValue(detail.WebsocketTimeline),
	)
	if err != nil {
		return false, fmt.Errorf("usage postgres repository: insert event: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("usage postgres repository: inspect inserted event: %w", err)
	}
	return rowsAffected > 0, nil
}

func (r *PostgresRepository) upsertRollupTx(ctx context.Context, tx *sql.Tx, table string, bucketStart time.Time, event UsageEvent) error {
	detail := event.Detail
	tokens := detail.Tokens
	success := int64(1)
	failed := int64(0)
	if detail.Failed {
		success = 0
		failed = 1
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s AS existing (
			bucket_start, provider, model, alias, account_label, api_key_label, endpoint,
			requests, success, failed, input_tokens, output_tokens, reasoning_tokens,
			cached_tokens, total_tokens, cost_usd, last_used
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, $11, $12, $13, $14, $15, $16
		)
		ON CONFLICT (bucket_start, provider, model, alias, account_label, api_key_label, endpoint)
		DO UPDATE SET
			requests = existing.requests + EXCLUDED.requests,
			success = existing.success + EXCLUDED.success,
			failed = existing.failed + EXCLUDED.failed,
			input_tokens = existing.input_tokens + EXCLUDED.input_tokens,
			output_tokens = existing.output_tokens + EXCLUDED.output_tokens,
			reasoning_tokens = existing.reasoning_tokens + EXCLUDED.reasoning_tokens,
			cached_tokens = existing.cached_tokens + EXCLUDED.cached_tokens,
			total_tokens = existing.total_tokens + EXCLUDED.total_tokens,
			cost_usd = existing.cost_usd + EXCLUDED.cost_usd,
			last_used = GREATEST(COALESCE(existing.last_used, EXCLUDED.last_used), EXCLUDED.last_used)
	`, r.fullTableName(table)),
		bucketStart.UTC(),
		detail.Provider,
		detail.Model,
		detail.Alias,
		detail.AccountLabel,
		detail.APIKeyLabel,
		detail.Endpoint,
		success,
		failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
		detail.CostUSD,
		detail.Timestamp.UTC(),
	)
	if err != nil {
		return fmt.Errorf("usage postgres repository: upsert rollup: %w", err)
	}
	return nil
}

func (r *PostgresRepository) UpdateEventDetail(ctx context.Context, detail RequestDetail) error {
	if r == nil || r.db == nil || strings.TrimSpace(detail.ID) == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s SET
			ttft_ms = $2,
			request_json = $3,
			provider_request_json = $4,
			provider_response_json = $5,
			response_json = $6,
			websocket_timeline_json = $7
		WHERE id = $1
	`, r.fullTableName(r.cfg.EventsTable)),
		detail.ID,
		detail.Latency.TTFTMs,
		jsonbValue(detail.Request),
		jsonbValue(detail.ProviderRequest),
		jsonbValue(detail.ProviderResponse),
		jsonbValue(detail.Response),
		jsonbValue(detail.WebsocketTimeline),
	)
	if err != nil {
		return fmt.Errorf("usage postgres repository: update event detail: %w", err)
	}
	return nil
}

func (r *PostgresRepository) SnapshotForKey(ctx context.Context, apiKeyHash string, keyLabel string, windowDays int, active bool, now time.Time, enabled bool) (Snapshot, error) {
	windowDays = normalizeWindowDays(windowDays)
	out := Snapshot{
		KeyLabel:               keyLabel,
		Active:                 active,
		UsageStatisticsEnabled: enabled,
		RetentionDays:          retentionDays,
		WindowDays:             windowDays,
		Series:                 make([]DailyPoint, 0, windowDays),
		RecentRequests:         make([]RecentRequest, 0),
	}
	if apiKeyHash == "" {
		return out, nil
	}
	today := startOfLocalDay(now)
	start := today.AddDate(0, 0, -(windowDays - 1))
	events, err := r.queryEvents(ctx, "timestamp >= $1 AND timestamp <= $2 AND api_key_hash = $3", []any{start.UTC(), now.UTC(), apiKeyHash}, "timestamp ASC", 0, 0)
	if err != nil {
		return Snapshot{}, err
	}
	daily := make(map[string]Aggregate)
	hourly := make(map[string]Aggregate)
	for _, event := range events {
		day := localDay(event.Request.Time)
		requestAggregate := aggregateFromRequest(event.Request)
		aggregate := daily[day]
		aggregate.add(requestAggregate)
		daily[day] = aggregate
		if windowDays == 1 {
			hour := localHour(event.Request.Time)
			hourAggregate := hourly[hour]
			hourAggregate.add(requestAggregate)
			hourly[hour] = hourAggregate
		}
		out.Totals.add(requestAggregate)
		if out.UpdatedAt == nil || event.Request.Time.After(*out.UpdatedAt) {
			updated := event.Request.Time
			out.UpdatedAt = &updated
		}
	}
	if windowDays == 1 {
		out.Series = make([]DailyPoint, 0, 24)
		for i := 0; i < 24; i++ {
			hour := start.Add(time.Duration(i) * time.Hour)
			key := hour.Format(time.RFC3339)
			out.Series = append(out.Series, DailyPoint{
				Date:      key,
				Label:     hour.Format("15:00"),
				Aggregate: hourly[key],
			})
		}
	} else {
		for i := 0; i < windowDays; i++ {
			day := start.AddDate(0, 0, i)
			key := day.Format("2006-01-02")
			out.Series = append(out.Series, DailyPoint{Date: key, Aggregate: daily[key]})
		}
	}
	for i := len(events) - 1; i >= 0 && len(out.RecentRequests) < maxRecentRequests; i-- {
		out.RecentRequests = append(out.RecentRequests, events[i].Request)
	}
	return out, nil
}

func (r *PostgresRepository) Analytics(ctx context.Context, period string, now time.Time, enabled bool, activeRequests []ActiveRequest) (AnalyticsSnapshot, error) {
	window := normalizeAnalyticsPeriod(period, now)
	recent, err := r.recentRequests(ctx, window, 20)
	if err != nil {
		return AnalyticsSnapshot{}, err
	}
	previousTotals, err := r.analyticsTotals(ctx, window.previous())
	if err != nil {
		return AnalyticsSnapshot{}, err
	}
	if r.shouldQueryRollups(ctx, window) {
		out, err := r.analyticsFromRollups(ctx, window, now, enabled, activeRequests, recent)
		if err != nil {
			return AnalyticsSnapshot{}, err
		}
		out.PreviousTotals = previousTotals
		return out, nil
	}
	events, err := r.queryEvents(ctx, "timestamp >= $1 AND timestamp <= $2", []any{window.start, window.end}, "timestamp ASC", 0, 0)
	if err != nil {
		return AnalyticsSnapshot{}, err
	}
	requests := make([]RecentRequest, 0, len(events))
	for _, event := range events {
		requests = append(requests, event.Request)
	}
	out := analyticsFromRequests(window, now, enabled, activeRequests, recent, requests)
	out.PreviousTotals = previousTotals
	return out, nil
}

func (r *PostgresRepository) RequestDetails(ctx context.Context, filter RequestDetailsFilter, now time.Time) (RequestDetailsSnapshot, error) {
	filter.normalize()
	where, args := r.requestDetailsWhere(filter, now)
	totals, totalItems, err := r.requestDetailsTotals(ctx, where, args)
	if err != nil {
		return RequestDetailsSnapshot{}, err
	}
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + filter.PageSize - 1) / filter.PageSize
	}
	offset := (filter.Page - 1) * filter.PageSize
	if offset > totalItems {
		offset = totalItems
	}
	details, err := r.queryDetails(ctx, where, args, filter.PageSize, offset)
	if err != nil {
		return RequestDetailsSnapshot{}, err
	}
	return RequestDetailsSnapshot{
		Details: details,
		Totals:  totals,
		Pagination: Pagination{
			Page:       filter.Page,
			PageSize:   filter.PageSize,
			TotalItems: totalItems,
			TotalPages: totalPages,
			HasNext:    totalPages > 0 && filter.Page < totalPages,
			HasPrev:    filter.Page > 1 && totalPages > 0,
		},
	}, nil
}

func (r *PostgresRepository) requestDetailsWhere(filter RequestDetailsFilter, now time.Time) (string, []any) {
	clauses := []string{"timestamp >= $1", "timestamp <= $2"}
	start := startOfLocalDay(now).AddDate(0, 0, -(retentionDays - 1)).UTC()
	if !filter.StartTime.IsZero() {
		start = filter.StartTime.UTC()
	}
	end := now.UTC()
	if !filter.EndTime.IsZero() {
		end = filter.EndTime.UTC()
	}
	args := []any{start, end}
	addILike := func(column string, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		args = append(args, "%"+value+"%")
		clauses = append(clauses, fmt.Sprintf("%s ILIKE $%d", column, len(args)))
	}
	addILike("provider", filter.Provider)
	addILike("model", filter.Model)
	addILike("endpoint", filter.Endpoint)
	switch strings.ToLower(strings.TrimSpace(filter.Status)) {
	case "success":
		clauses = append(clauses, "failed = FALSE")
	case "failed":
		clauses = append(clauses, "failed = TRUE")
	}
	if apiKey := strings.TrimSpace(filter.APIKey); apiKey != "" {
		args = append(args, hashAPIKey(apiKey), "%"+apiKey+"%")
		clauses = append(clauses, fmt.Sprintf("(api_key_hash = $%d OR api_key_label ILIKE $%d)", len(args)-1, len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

func (r *PostgresRepository) requestDetailsTotals(ctx context.Context, where string, args []any) (Aggregate, int, error) {
	query := fmt.Sprintf(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN failed THEN 0 ELSE 1 END), 0),
			COALESCE(SUM(CASE WHEN failed THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM %s WHERE %s
	`, r.fullTableName(r.cfg.EventsTable), where)
	var totalItems int
	var total Aggregate
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&totalItems,
		&total.Success,
		&total.Failed,
		&total.Tokens.InputTokens,
		&total.Tokens.OutputTokens,
		&total.Tokens.ReasoningTokens,
		&total.Tokens.CachedTokens,
		&total.Tokens.CacheReadTokens,
		&total.Tokens.CacheCreationTokens,
		&total.Tokens.TotalTokens,
		&total.CostUSD,
	)
	if err != nil {
		return Aggregate{}, 0, fmt.Errorf("usage postgres repository: query request detail totals: %w", err)
	}
	total.Requests = int64(totalItems)
	return total, totalItems, nil
}

func (r *PostgresRepository) analyticsTotals(ctx context.Context, window analyticsPeriod) (Aggregate, error) {
	var total Aggregate
	if window.empty {
		return total, nil
	}
	query := fmt.Sprintf(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN failed THEN 0 ELSE 1 END), 0),
			COALESCE(SUM(CASE WHEN failed THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM %s WHERE timestamp >= $1 AND timestamp <= $2
	`, r.fullTableName(r.cfg.EventsTable))
	var totalItems int64
	err := r.db.QueryRowContext(ctx, query, window.start, window.end).Scan(
		&totalItems,
		&total.Success,
		&total.Failed,
		&total.Tokens.InputTokens,
		&total.Tokens.OutputTokens,
		&total.Tokens.ReasoningTokens,
		&total.Tokens.CachedTokens,
		&total.Tokens.CacheReadTokens,
		&total.Tokens.CacheCreationTokens,
		&total.Tokens.TotalTokens,
		&total.CostUSD,
	)
	if err != nil {
		return Aggregate{}, fmt.Errorf("usage postgres repository: query analytics previous totals: %w", err)
	}
	total.Requests = totalItems
	return total, nil
}

func (r *PostgresRepository) queryDetails(ctx context.Context, where string, args []any, limit int, offset int) ([]RequestDetail, error) {
	queryArgs := append([]any(nil), args...)
	queryArgs = append(queryArgs, limit, offset)
	query := fmt.Sprintf(`
		SELECT %s FROM %s WHERE %s ORDER BY timestamp DESC LIMIT $%d OFFSET $%d
	`, eventSelectColumns(), r.fullTableName(r.cfg.EventsTable), where, len(queryArgs)-1, len(queryArgs))
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("usage postgres repository: query request details: %w", err)
	}
	defer rows.Close()
	events, err := scanUsageEvents(rows)
	if err != nil {
		return nil, err
	}
	out := make([]RequestDetail, 0, len(events))
	for _, event := range events {
		out = append(out, event.Detail)
	}
	return out, nil
}

func (r *PostgresRepository) recentRequests(ctx context.Context, window analyticsPeriod, limit int) ([]RecentRequest, error) {
	events, err := r.queryEvents(ctx, "timestamp >= $1 AND timestamp <= $2", []any{window.start, window.end}, "timestamp DESC", limit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]RecentRequest, 0, len(events))
	for _, event := range events {
		out = append(out, event.Request)
	}
	return out, nil
}

func (r *PostgresRepository) queryEvents(ctx context.Context, where string, args []any, order string, limit int, offset int) ([]UsageEvent, error) {
	queryArgs := append([]any(nil), args...)
	limitClause := ""
	if strings.TrimSpace(order) == "" {
		order = "timestamp ASC"
	}
	if limit > 0 {
		queryArgs = append(queryArgs, limit, offset)
		limitClause = fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(queryArgs)-1, len(queryArgs))
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY %s%s",
		eventSelectColumns(),
		r.fullTableName(r.cfg.EventsTable),
		where,
		order,
		limitClause,
	)
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("usage postgres repository: query events: %w", err)
	}
	defer rows.Close()
	return scanUsageEvents(rows)
}

func (r *PostgresRepository) shouldQueryRollups(ctx context.Context, window analyticsPeriod) bool {
	if r == nil || !r.cfg.RollupsEnabled {
		return false
	}
	if r.cfg.RollupQueryMinEvents <= 0 {
		return true
	}
	var count int64
	err := r.db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM %s WHERE timestamp >= $1 AND timestamp <= $2",
		r.fullTableName(r.cfg.EventsTable),
	), window.start, window.end).Scan(&count)
	if err != nil {
		return false
	}
	return count >= r.cfg.RollupQueryMinEvents
}

func (r *PostgresRepository) analyticsFromRollups(ctx context.Context, window analyticsPeriod, now time.Time, enabled bool, activeRequests []ActiveRequest, recent []RecentRequest) (AnalyticsSnapshot, error) {
	table := r.cfg.DailyRollupTable
	if window.hourly {
		table = r.cfg.HourlyRollupTable
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT bucket_start, provider, model, alias, account_label, api_key_label, endpoint,
			requests, success, failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			total_tokens, cost_usd, last_used
		FROM %s
		WHERE bucket_start >= $1 AND bucket_start <= $2
		ORDER BY bucket_start ASC
	`, r.fullTableName(table)), window.start, window.end)
	if err != nil {
		return AnalyticsSnapshot{}, fmt.Errorf("usage postgres repository: query rollups: %w", err)
	}
	defer rows.Close()

	out := baseAnalyticsSnapshot(window, enabled, activeRequests, recent)
	providers := make(map[string]*AnalyticsGroup)
	models := make(map[string]*AnalyticsGroup)
	accounts := make(map[string]*AnalyticsGroup)
	apiKeys := make(map[string]*AnalyticsGroup)
	endpoints := make(map[string]*AnalyticsGroup)
	hourlyBuckets := window.chartBuckets()
	dailyBuckets := make(map[string]*ChartPoint, len(window.days))
	if !window.hourly {
		for _, day := range window.days {
			dailyBuckets[day] = &ChartPoint{Label: shortDateLabel(day), Date: day}
		}
	}

	for rows.Next() {
		var row rollupRow
		var lastUsed sql.NullTime
		if err := rows.Scan(
			&row.BucketStart,
			&row.Provider,
			&row.Model,
			&row.Alias,
			&row.AccountLabel,
			&row.APIKeyLabel,
			&row.Endpoint,
			&row.Aggregate.Requests,
			&row.Aggregate.Success,
			&row.Aggregate.Failed,
			&row.Aggregate.Tokens.InputTokens,
			&row.Aggregate.Tokens.OutputTokens,
			&row.Aggregate.Tokens.ReasoningTokens,
			&row.Aggregate.Tokens.CachedTokens,
			&row.Aggregate.Tokens.TotalTokens,
			&row.Aggregate.CostUSD,
			&lastUsed,
		); err != nil {
			return AnalyticsSnapshot{}, fmt.Errorf("usage postgres repository: scan rollup: %w", err)
		}
		if lastUsed.Valid {
			row.LastUsed = lastUsed.Time.UTC()
		} else {
			row.LastUsed = row.BucketStart.UTC()
		}
		if out.UpdatedAt == nil || row.LastUsed.After(*out.UpdatedAt) {
			updated := row.LastUsed
			out.UpdatedAt = &updated
		}
		out.Totals.add(row.Aggregate)
		if window.hourly {
			if bucket := window.bucketFor(row.BucketStart, hourlyBuckets); bucket != nil {
				bucket.addAggregate(row.Aggregate)
			}
		} else {
			day := localDay(row.BucketStart)
			if bucket := dailyBuckets[day]; bucket != nil {
				bucket.addAggregate(row.Aggregate)
			}
		}
		request := row.request()
		addRequestGroup(providers, providerGroupKey(request), request, row.Aggregate)
		addRequestGroup(models, modelGroupKey(request), request, row.Aggregate)
		addRequestGroup(accounts, accountGroupKey(request), request, row.Aggregate)
		addRequestGroup(apiKeys, apiKeyGroupKey(request), request, row.Aggregate)
		addRequestGroup(endpoints, endpointGroupKey(request), request, row.Aggregate)
	}
	if err := rows.Err(); err != nil {
		return AnalyticsSnapshot{}, fmt.Errorf("usage postgres repository: iterate rollups: %w", err)
	}

	if window.hourly {
		out.Series = append(out.Series, hourlyBuckets...)
	} else {
		for _, day := range window.days {
			if bucket := dailyBuckets[day]; bucket != nil {
				out.Series = append(out.Series, *bucket)
			}
		}
	}
	out.ByProvider = sortedGroups(providers)
	out.ByModel = sortedGroups(models)
	out.ByAccount = sortedGroups(accounts)
	out.ByAPIKey = sortedGroups(apiKeys)
	out.ByEndpoint = sortedGroups(endpoints)
	return out, nil
}

type rollupRow struct {
	BucketStart  time.Time
	Provider     string
	Model        string
	Alias        string
	AccountLabel string
	APIKeyLabel  string
	Endpoint     string
	Aggregate    Aggregate
	LastUsed     time.Time
}

func (r rollupRow) request() RecentRequest {
	return RecentRequest{
		Time:            r.LastUsed,
		Provider:        r.Provider,
		Model:           r.Model,
		Alias:           r.Alias,
		AccountLabel:    r.AccountLabel,
		APIKeyLabel:     r.APIKeyLabel,
		Endpoint:        r.Endpoint,
		InputTokens:     r.Aggregate.Tokens.InputTokens,
		OutputTokens:    r.Aggregate.Tokens.OutputTokens,
		ReasoningTokens: r.Aggregate.Tokens.ReasoningTokens,
		CachedTokens:    r.Aggregate.Tokens.CachedTokens,
		TotalTokens:     r.Aggregate.Tokens.TotalTokens,
		CostUSD:         r.Aggregate.CostUSD,
	}
}

func baseAnalyticsSnapshot(window analyticsPeriod, enabled bool, activeRequests []ActiveRequest, recent []RecentRequest) AnalyticsSnapshot {
	return AnalyticsSnapshot{
		Period:                 window.period,
		UsageStatisticsEnabled: enabled,
		RetentionDays:          retentionDays,
		Series:                 make([]ChartPoint, 0, window.bucketCount),
		ActiveRequests:         activeRequests,
		RecentRequests:         recent,
	}
}

func analyticsFromRequests(window analyticsPeriod, now time.Time, enabled bool, activeRequests []ActiveRequest, recent []RecentRequest, requests []RecentRequest) AnalyticsSnapshot {
	out := baseAnalyticsSnapshot(window, enabled, activeRequests, recent)
	providers := make(map[string]*AnalyticsGroup)
	models := make(map[string]*AnalyticsGroup)
	accounts := make(map[string]*AnalyticsGroup)
	apiKeys := make(map[string]*AnalyticsGroup)
	endpoints := make(map[string]*AnalyticsGroup)

	if window.hourly {
		buckets := window.chartBuckets()
		for _, request := range requests {
			if !window.includesTime(request.Time) {
				continue
			}
			aggregate := aggregateFromRequest(request)
			out.Totals.add(aggregate)
			if bucket := window.bucketFor(request.Time, buckets); bucket != nil {
				bucket.addAggregate(aggregate)
			}
			addRequestGroup(providers, providerGroupKey(request), request, aggregate)
			addRequestGroup(models, modelGroupKey(request), request, aggregate)
			addRequestGroup(accounts, accountGroupKey(request), request, aggregate)
			addRequestGroup(apiKeys, apiKeyGroupKey(request), request, aggregate)
			addRequestGroup(endpoints, endpointGroupKey(request), request, aggregate)
			if out.UpdatedAt == nil || request.Time.After(*out.UpdatedAt) {
				updated := request.Time
				out.UpdatedAt = &updated
			}
		}
		out.Series = append(out.Series, buckets...)
	} else {
		daily := make(map[string]*dayUsage)
		for _, request := range requests {
			if !window.includesTime(request.Time) {
				continue
			}
			day := localDay(request.Time)
			entry := daily[day]
			if entry == nil {
				entry = newDayUsage()
				daily[day] = entry
			}
			aggregate := aggregateFromRequest(request)
			entry.add(request, aggregate)
			if out.UpdatedAt == nil || request.Time.After(*out.UpdatedAt) {
				updated := request.Time
				out.UpdatedAt = &updated
			}
		}
		for _, day := range window.days {
			aggregate := Aggregate{}
			if entry := daily[day]; entry != nil {
				aggregate = entry.Aggregate
				out.Totals.add(entry.Aggregate)
				mergeGroupMap(providers, entry.ByProvider)
				mergeGroupMap(models, entry.ByModel)
				mergeGroupMap(accounts, entry.ByAccount)
				mergeGroupMap(apiKeys, entry.ByAPIKey)
				mergeGroupMap(endpoints, entry.ByEndpoint)
			}
			out.Series = append(out.Series, ChartPoint{
				Label:     shortDateLabel(day),
				Date:      day,
				Requests:  aggregate.Requests,
				Tokens:    aggregate.Tokens.TotalTokens,
				Breakdown: aggregate.Tokens,
				CostUSD:   aggregate.CostUSD,
			})
		}
	}
	out.ByProvider = sortedGroups(providers)
	out.ByModel = sortedGroups(models)
	out.ByAccount = sortedGroups(accounts)
	out.ByAPIKey = sortedGroups(apiKeys)
	out.ByEndpoint = sortedGroups(endpoints)
	return out
}

func eventSelectColumns() string {
	return strings.Join([]string{
		"id",
		"request_id",
		"timestamp",
		"provider",
		"model",
		"alias",
		"source",
		"account_label",
		"api_key_label",
		"api_key_hash",
		"auth_index",
		"auth_type",
		"endpoint",
		"reasoning_effort",
		"status",
		"status_code",
		"failed",
		"latency_ms",
		"ttft_ms",
		"input_tokens",
		"output_tokens",
		"reasoning_tokens",
		"cached_tokens",
		"cache_read_tokens",
		"cache_creation_tokens",
		"total_tokens",
		"cost_usd",
		"request_json",
		"provider_request_json",
		"provider_response_json",
		"response_json",
		"websocket_timeline_json",
	}, ", ")
}

func scanUsageEvents(rows *sql.Rows) ([]UsageEvent, error) {
	events := make([]UsageEvent, 0)
	for rows.Next() {
		var event UsageEvent
		var requestJSON, providerRequestJSON, providerResponseJSON, responseJSON, websocketJSON []byte
		if err := rows.Scan(
			&event.Detail.ID,
			&event.Detail.RequestID,
			&event.Detail.Timestamp,
			&event.Detail.Provider,
			&event.Detail.Model,
			&event.Detail.Alias,
			&event.Request.Source,
			&event.Detail.AccountLabel,
			&event.Detail.APIKeyLabel,
			&event.APIKeyHash,
			&event.Request.AuthIndex,
			&event.Request.AuthType,
			&event.Detail.Endpoint,
			&event.Detail.ReasoningEffort,
			&event.Detail.Status,
			&event.Detail.StatusCode,
			&event.Detail.Failed,
			&event.Detail.Latency.TotalMs,
			&event.Detail.Latency.TTFTMs,
			&event.Detail.Tokens.InputTokens,
			&event.Detail.Tokens.OutputTokens,
			&event.Detail.Tokens.ReasoningTokens,
			&event.Detail.Tokens.CachedTokens,
			&event.Detail.Tokens.CacheReadTokens,
			&event.Detail.Tokens.CacheCreationTokens,
			&event.Detail.Tokens.TotalTokens,
			&event.Detail.CostUSD,
			&requestJSON,
			&providerRequestJSON,
			&providerResponseJSON,
			&responseJSON,
			&websocketJSON,
		); err != nil {
			return nil, fmt.Errorf("usage postgres repository: scan event: %w", err)
		}
		event.Detail.Request = decodeJSONB(requestJSON, &HTTPMessageDetail{})
		event.Detail.ProviderRequest = decodeJSONB(providerRequestJSON, nil)
		event.Detail.ProviderResponse = decodeJSONB(providerResponseJSON, nil)
		event.Detail.Response = decodeJSONB(responseJSON, &HTTPResponseDetail{})
		event.Detail.WebsocketTimeline = decodeJSONB(websocketJSON, nil)
		event.Request = requestFromDetail(event.Detail, event.Request.Source, event.Request.AuthIndex, event.Request.AuthType)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage postgres repository: iterate events: %w", err)
	}
	return events, nil
}

func requestFromDetail(detail RequestDetail, source string, authIndex string, authType string) RecentRequest {
	return RecentRequest{
		Time:            detail.Timestamp.UTC(),
		Provider:        detail.Provider,
		Model:           detail.Model,
		Alias:           detail.Alias,
		Source:          source,
		AccountLabel:    detail.AccountLabel,
		APIKeyLabel:     detail.APIKeyLabel,
		AuthIndex:       authIndex,
		AuthType:        authType,
		Endpoint:        detail.Endpoint,
		RequestID:       detail.RequestID,
		ReasoningEffort: detail.ReasoningEffort,
		InputTokens:     detail.Tokens.InputTokens,
		OutputTokens:    detail.Tokens.OutputTokens,
		ReasoningTokens: detail.Tokens.ReasoningTokens,
		CachedTokens:    detail.Tokens.CachedTokens,
		TotalTokens:     detail.Tokens.TotalTokens,
		CostUSD:         detail.CostUSD,
		LatencyMs:       detail.Latency.TotalMs,
		StatusCode:      detail.StatusCode,
		Failed:          detail.Failed,
	}
}

func jsonbValue(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

func decodeJSONB(raw []byte, typed any) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if typed != nil {
		if err := json.Unmarshal(raw, typed); err == nil {
			return typed
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	return value
}

func (r *PostgresRepository) fullTableName(name string) string {
	if strings.TrimSpace(r.cfg.Schema) == "" {
		return quoteSQLIdentifier(name)
	}
	return quoteSQLIdentifier(r.cfg.Schema) + "." + quoteSQLIdentifier(name)
}

func (r *PostgresRepository) indexName(name string) string {
	if strings.TrimSpace(r.cfg.Schema) == "" {
		return name
	}
	return strings.TrimSpace(r.cfg.Schema) + "_" + name
}

func quoteSQLIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "\"", "\"\"")
	return "\"" + replaced + "\""
}
