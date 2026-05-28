package usageportal

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestPostgresRepositoryPersistsQueriesAndRollups(t *testing.T) {
	dsn := os.Getenv("USAGEPORTAL_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set USAGEPORTAL_POSTGRES_TEST_DSN to run postgres integration test")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("usageportal_test_%d", time.Now().UnixNano())
	repo, err := NewPostgresRepository(ctx, PostgresRepositoryConfig{
		DSN:                  dsn,
		Schema:               schema,
		RollupsEnabled:       true,
		RollupQueryMinEvents: 1,
	})
	if err != nil {
		t.Fatalf("new postgres repository: %v", err)
	}
	defer repo.Close()
	defer func() {
		if _, errDrop := repo.db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", quoteSQLIdentifier(schema))); errDrop != nil {
			t.Fatalf("drop test schema: %v", errDrop)
		}
	}()

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.Local)
	record := coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-5.5",
		Alias:       "client-model",
		APIKey:      "sk-test-key-123456",
		Source:      "account-a",
		RequestedAt: now,
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 4,
			TotalTokens:  7,
		},
	}
	request, _ := requestFromRecord(internallogging.WithRequestID(context.Background(), "req_pg"), record)
	detail := requestDetailFromRecord(request, record, HTTPRequestDetail{})
	if err := repo.InsertEvent(ctx, UsageEvent{APIKeyHash: hashAPIKey(record.APIKey), Request: request, Detail: detail}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	analytics, err := repo.Analytics(ctx, "today", now.Add(time.Minute), true, nil)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if analytics.Totals.Requests != 1 || analytics.Totals.Tokens.TotalTokens != 7 {
		t.Fatalf("analytics totals = %+v, want 1 request / 7 tokens", analytics.Totals)
	}
	if len(analytics.ByProvider) != 1 || analytics.ByProvider[0].Provider != "openai" {
		t.Fatalf("analytics provider groups = %+v, want openai", analytics.ByProvider)
	}

	snapshot, err := repo.SnapshotForKey(ctx, hashAPIKey(record.APIKey), MaskAPIKey(record.APIKey), 7, true, now.Add(time.Minute), true)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Totals.Requests != 1 || len(snapshot.RecentRequests) != 1 {
		t.Fatalf("snapshot = %+v, want persisted key usage", snapshot)
	}

	details, err := repo.RequestDetails(ctx, RequestDetailsFilter{Page: 1, PageSize: 10, APIKey: record.APIKey}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("request details: %v", err)
	}
	if details.Totals.Requests != 1 || len(details.Details) != 1 {
		t.Fatalf("details = %+v, want one event", details)
	}

	detail.Response = &HTTPResponseDetail{StatusCode: 200, Body: map[string]any{"ok": true}}
	detail.Latency.TTFTMs = 123
	if err := repo.UpdateEventDetail(ctx, detail); err != nil {
		t.Fatalf("update detail: %v", err)
	}
	details, err = repo.RequestDetails(ctx, RequestDetailsFilter{Page: 1, PageSize: 10}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("request details after update: %v", err)
	}
	response, ok := details.Details[0].Response.(*HTTPResponseDetail)
	if !ok || response.StatusCode != 200 {
		t.Fatalf("response detail = %#v, want persisted HTTP response", details.Details[0].Response)
	}
	if details.Details[0].Latency.TTFTMs != 123 {
		t.Fatalf("ttft = %d, want 123", details.Details[0].Latency.TTFTMs)
	}
}
