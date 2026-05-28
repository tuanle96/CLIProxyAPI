package apikeypolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usageportal"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const oneTimePeriodKey = "one_time"
const defaultPostgresQuotaLedgerTable = "api_key_quota_ledger"

type LedgerKey struct {
	APIKeyHash string
	Period     string
	PeriodKey  string
}

type QuotaUsage struct {
	TotalTokens int64     `json:"total_tokens"`
	CostUSD     float64   `json:"cost_usd"`
	Requests    int64     `json:"requests"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type QuotaMetricInt struct {
	Used      int64 `json:"used"`
	Limit     int64 `json:"limit"`
	Remaining int64 `json:"remaining"`
	Exceeded  bool  `json:"exceeded"`
	Unlimited bool  `json:"unlimited"`
}

type QuotaMetricFloat struct {
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	Exceeded  bool    `json:"exceeded"`
	Unlimited bool    `json:"unlimited"`
}

type QuotaStatus struct {
	Period          string           `json:"period"`
	PeriodKey       string           `json:"period_key"`
	Requests        int64            `json:"requests"`
	TokenQuota      QuotaMetricInt   `json:"token_quota"`
	USDQuota        QuotaMetricFloat `json:"usd_quota"`
	ExceededMetrics []string         `json:"exceeded_metrics"`
	Blocked         bool             `json:"blocked"`
	StoreAvailable  bool             `json:"store_available"`
}

type Ledger interface {
	Get(ctx context.Context, key LedgerKey) (QuotaUsage, error)
	Add(ctx context.Context, key LedgerKey, delta QuotaUsage) error
	Close() error
}

type manager struct {
	mu     sync.RWMutex
	ledger Ledger
}

var defaultManager = &manager{ledger: newMemoryLedger()}

func init() {
	coreusage.RegisterPlugin(defaultManager)
}

func ConfigureFileLedger(authDir string) error {
	ledger, err := newFileLedger(filepath.Join(strings.TrimSpace(authDir), "quota-ledger.json"))
	defaultManager.mu.Lock()
	old := defaultManager.ledger
	if err != nil {
		defaultManager.ledger = unavailableLedger{err: err}
	} else {
		defaultManager.ledger = ledger
	}
	defaultManager.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return err
}

type PostgresLedgerConfig struct {
	DSN    string
	Schema string
	Table  string
}

func ConfigurePostgresLedger(ctx context.Context, cfg PostgresLedgerConfig) error {
	ledger, err := newPostgresLedger(ctx, cfg)
	defaultManager.mu.Lock()
	old := defaultManager.ledger
	if err != nil {
		defaultManager.ledger = unavailableLedger{err: err}
	} else {
		defaultManager.ledger = ledger
	}
	defaultManager.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return err
}

func ResetForTesting() {
	defaultManager.mu.Lock()
	old := defaultManager.ledger
	defaultManager.ledger = newMemoryLedger()
	defaultManager.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func QuotaConfigured(meta internalconfig.APIKeyMetadata) bool {
	meta = internalconfig.NormalizeAPIKeyMetadata(meta)
	return meta.TokenQuotaLimit > 0 || meta.USDQuotaLimit > 0
}

func StatusForAPIKey(apiKey string, meta internalconfig.APIKeyMetadata, now time.Time) (QuotaStatus, error) {
	return StatusForAPIKeyID(internalconfig.APIKeyID(apiKey), meta, now)
}

func StatusForAPIKeyID(apiKeyID string, meta internalconfig.APIKeyMetadata, now time.Time) (QuotaStatus, error) {
	meta = internalconfig.NormalizeAPIKeyMetadata(meta)
	period := effectiveQuotaPeriod(meta)
	periodKey := quotaPeriodKey(period, now)
	status := QuotaStatus{
		Period:         period,
		PeriodKey:      periodKey,
		StoreAvailable: true,
		TokenQuota: QuotaMetricInt{
			Limit:     meta.TokenQuotaLimit,
			Unlimited: meta.TokenQuotaLimit <= 0,
		},
		USDQuota: QuotaMetricFloat{
			Limit:     meta.USDQuotaLimit,
			Unlimited: meta.USDQuotaLimit <= 0,
		},
	}
	usage, err := defaultManager.get(context.Background(), LedgerKey{
		APIKeyHash: strings.TrimSpace(apiKeyID),
		Period:     period,
		PeriodKey:  periodKey,
	})
	if err != nil {
		status.StoreAvailable = false
		return status, err
	}
	status.Requests = usage.Requests
	status.TokenQuota.Used = usage.TotalTokens
	status.USDQuota.Used = usage.CostUSD
	if meta.TokenQuotaLimit > 0 {
		status.TokenQuota.Remaining = meta.TokenQuotaLimit - usage.TotalTokens
		if status.TokenQuota.Remaining < 0 {
			status.TokenQuota.Remaining = 0
		}
		status.TokenQuota.Exceeded = usage.TotalTokens >= meta.TokenQuotaLimit
		if status.TokenQuota.Exceeded {
			status.ExceededMetrics = append(status.ExceededMetrics, "tokens")
		}
	}
	if meta.USDQuotaLimit > 0 {
		status.USDQuota.Remaining = meta.USDQuotaLimit - usage.CostUSD
		if status.USDQuota.Remaining < 0 {
			status.USDQuota.Remaining = 0
		}
		status.USDQuota.Exceeded = usage.CostUSD >= meta.USDQuotaLimit
		if status.USDQuota.Exceeded {
			status.ExceededMetrics = append(status.ExceededMetrics, "usd")
		}
	}
	status.Blocked = len(status.ExceededMetrics) > 0
	return status, nil
}

func (m *manager) HandleUsage(ctx context.Context, record coreusage.Record) {
	apiKeyID := internalconfig.APIKeyID(record.APIKey)
	if apiKeyID == "" {
		return
	}
	now := record.RequestedAt
	if now.IsZero() {
		now = time.Now()
	}
	delta := QuotaUsage{
		TotalTokens: usageportal.RecordTotalTokens(record),
		CostUSD:     usageportal.EstimateRecordCostUSD(record),
		Requests:    1,
		UpdatedAt:   now.UTC(),
	}
	if delta.TotalTokens == 0 && delta.CostUSD == 0 && delta.Requests == 0 {
		return
	}
	keys := []LedgerKey{
		{APIKeyHash: apiKeyID, Period: internalconfig.APIKeyQuotaPeriodDaily, PeriodKey: quotaPeriodKey(internalconfig.APIKeyQuotaPeriodDaily, now)},
		{APIKeyHash: apiKeyID, Period: internalconfig.APIKeyQuotaPeriodOneTime, PeriodKey: oneTimePeriodKey},
	}
	for _, key := range keys {
		if err := m.add(ctx, key, delta); err != nil {
			return
		}
	}
}

func (m *manager) get(ctx context.Context, key LedgerKey) (QuotaUsage, error) {
	if m == nil {
		return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
	}
	m.mu.RLock()
	ledger := m.ledger
	m.mu.RUnlock()
	if ledger == nil {
		return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
	}
	return ledger.Get(ctx, key)
}

func (m *manager) add(ctx context.Context, key LedgerKey, delta QuotaUsage) error {
	if m == nil {
		return fmt.Errorf("quota ledger unavailable")
	}
	m.mu.RLock()
	ledger := m.ledger
	m.mu.RUnlock()
	if ledger == nil {
		return fmt.Errorf("quota ledger unavailable")
	}
	return ledger.Add(ctx, key, delta)
}

func effectiveQuotaPeriod(meta internalconfig.APIKeyMetadata) string {
	period := strings.ToLower(strings.TrimSpace(meta.QuotaPeriod))
	if period == internalconfig.APIKeyQuotaPeriodDaily || period == internalconfig.APIKeyQuotaPeriodOneTime {
		return period
	}
	if meta.DailyTokenLimit > 0 && meta.TokenQuotaLimit == meta.DailyTokenLimit {
		return internalconfig.APIKeyQuotaPeriodDaily
	}
	return internalconfig.APIKeyQuotaPeriodOneTime
}

func quotaPeriodKey(period string, now time.Time) string {
	if strings.EqualFold(period, internalconfig.APIKeyQuotaPeriodDaily) {
		if now.IsZero() {
			now = time.Now()
		}
		return now.In(time.Local).Format("2006-01-02")
	}
	return oneTimePeriodKey
}

func ledgerEntryKey(key LedgerKey) string {
	return strings.TrimSpace(key.APIKeyHash) + "|" + strings.TrimSpace(key.Period) + "|" + strings.TrimSpace(key.PeriodKey)
}

type unavailableLedger struct {
	err error
}

func (u unavailableLedger) Get(context.Context, LedgerKey) (QuotaUsage, error) {
	if u.err != nil {
		return QuotaUsage{}, u.err
	}
	return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
}

func (u unavailableLedger) Add(context.Context, LedgerKey, QuotaUsage) error {
	if u.err != nil {
		return u.err
	}
	return fmt.Errorf("quota ledger unavailable")
}

func (u unavailableLedger) Close() error { return nil }

type memoryLedger struct {
	mu      sync.Mutex
	entries map[string]QuotaUsage
}

func newMemoryLedger() *memoryLedger {
	return &memoryLedger{entries: make(map[string]QuotaUsage)}
}

func (l *memoryLedger) Get(_ context.Context, key LedgerKey) (QuotaUsage, error) {
	if l == nil {
		return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[ledgerEntryKey(key)], nil
}

func (l *memoryLedger) Add(_ context.Context, key LedgerKey, delta QuotaUsage) error {
	if l == nil {
		return fmt.Errorf("quota ledger unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entryKey := ledgerEntryKey(key)
	current := l.entries[entryKey]
	current.TotalTokens += delta.TotalTokens
	current.CostUSD += delta.CostUSD
	current.Requests += delta.Requests
	current.UpdatedAt = delta.UpdatedAt
	if current.UpdatedAt.IsZero() {
		current.UpdatedAt = time.Now().UTC()
	}
	l.entries[entryKey] = current
	return nil
}

func (l *memoryLedger) Close() error { return nil }

type fileLedger struct {
	mu      sync.Mutex
	path    string
	entries map[string]QuotaUsage
}

type fileLedgerData struct {
	Entries map[string]QuotaUsage `json:"entries"`
}

func newFileLedger(path string) (*fileLedger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("quota ledger path is empty")
	}
	ledger := &fileLedger{path: path, entries: make(map[string]QuotaUsage)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
				return nil, fmt.Errorf("quota ledger mkdir: %w", errMkdir)
			}
			return ledger, nil
		}
		return nil, fmt.Errorf("quota ledger read: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return ledger, nil
	}
	var parsed fileLedgerData
	if err = json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("quota ledger parse: %w", err)
	}
	if parsed.Entries != nil {
		ledger.entries = parsed.Entries
	}
	return ledger, nil
}

func (l *fileLedger) Get(_ context.Context, key LedgerKey) (QuotaUsage, error) {
	if l == nil {
		return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[ledgerEntryKey(key)], nil
}

func (l *fileLedger) Add(_ context.Context, key LedgerKey, delta QuotaUsage) error {
	if l == nil {
		return fmt.Errorf("quota ledger unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entryKey := ledgerEntryKey(key)
	current := l.entries[entryKey]
	current.TotalTokens += delta.TotalTokens
	current.CostUSD += delta.CostUSD
	current.Requests += delta.Requests
	current.UpdatedAt = delta.UpdatedAt
	if current.UpdatedAt.IsZero() {
		current.UpdatedAt = time.Now().UTC()
	}
	l.entries[entryKey] = current
	if err := l.persistLocked(); err != nil {
		return err
	}
	return nil
}

func (l *fileLedger) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("quota ledger mkdir: %w", err)
	}
	data, err := json.MarshalIndent(fileLedgerData{Entries: l.entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("quota ledger marshal: %w", err)
	}
	tmp := l.path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("quota ledger write: %w", err)
	}
	if err = os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("quota ledger rename: %w", err)
	}
	return nil
}

func (l *fileLedger) Close() error { return nil }

type postgresLedger struct {
	db       *sql.DB
	table    string
	tableRef string
}

func newPostgresLedger(ctx context.Context, cfg PostgresLedgerConfig) (*postgresLedger, error) {
	cfg.DSN = strings.TrimSpace(cfg.DSN)
	if cfg.DSN == "" {
		return nil, fmt.Errorf("quota postgres ledger: DSN is required")
	}
	if cfg.Table == "" {
		cfg.Table = defaultPostgresQuotaLedgerTable
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("quota postgres ledger: open database connection: %w", err)
	}
	ledger := &postgresLedger{db: db, table: fullTableName(cfg.Schema, cfg.Table), tableRef: quoteSQLIdentifier(cfg.Table)}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("quota postgres ledger: ping database: %w", err)
	}
	if schema := strings.TrimSpace(cfg.Schema); schema != "" {
		if _, err = db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteSQLIdentifier(schema))); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("quota postgres ledger: create schema: %w", err)
		}
	}
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			api_key_hash TEXT NOT NULL,
			quota_period TEXT NOT NULL,
			period_key TEXT NOT NULL,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			requests BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (api_key_hash, quota_period, period_key)
		)
	`, ledger.table)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("quota postgres ledger: create table: %w", err)
	}
	return ledger, nil
}

func (l *postgresLedger) Get(ctx context.Context, key LedgerKey) (QuotaUsage, error) {
	if l == nil || l.db == nil {
		return QuotaUsage{}, fmt.Errorf("quota ledger unavailable")
	}
	var usage QuotaUsage
	err := l.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT total_tokens, cost_usd, requests, updated_at
		FROM %s
		WHERE api_key_hash = $1 AND quota_period = $2 AND period_key = $3
	`, l.table), key.APIKeyHash, key.Period, key.PeriodKey).Scan(&usage.TotalTokens, &usage.CostUSD, &usage.Requests, &usage.UpdatedAt)
	if err == sql.ErrNoRows {
		return QuotaUsage{}, nil
	}
	if err != nil {
		return QuotaUsage{}, fmt.Errorf("quota postgres ledger: read: %w", err)
	}
	return usage, nil
}

func (l *postgresLedger) Add(ctx context.Context, key LedgerKey, delta QuotaUsage) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("quota ledger unavailable")
	}
	updatedAt := delta.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	_, err := l.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (api_key_hash, quota_period, period_key, total_tokens, cost_usd, requests, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (api_key_hash, quota_period, period_key)
		DO UPDATE SET
			total_tokens = %s.total_tokens + EXCLUDED.total_tokens,
			cost_usd = %s.cost_usd + EXCLUDED.cost_usd,
			requests = %s.requests + EXCLUDED.requests,
			updated_at = EXCLUDED.updated_at
	`, l.table, l.tableRef, l.tableRef, l.tableRef), key.APIKeyHash, key.Period, key.PeriodKey, delta.TotalTokens, delta.CostUSD, delta.Requests, updatedAt)
	if err != nil {
		return fmt.Errorf("quota postgres ledger: upsert: %w", err)
	}
	return nil
}

func (l *postgresLedger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

func fullTableName(schema, table string) string {
	if strings.TrimSpace(schema) == "" {
		return quoteSQLIdentifier(table)
	}
	return quoteSQLIdentifier(schema) + "." + quoteSQLIdentifier(table)
}

func quoteSQLIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "\"", "\"\"")
	return "\"" + replaced + "\""
}
