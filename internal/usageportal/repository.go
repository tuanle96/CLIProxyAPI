package usageportal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	repositoryInsertTimeout = 2 * time.Second
	repositoryQueryTimeout  = 5 * time.Second
)

type UsageEvent struct {
	APIKeyHash string
	Request    RecentRequest
	Detail     RequestDetail
}

type Repository interface {
	InsertEvent(ctx context.Context, event UsageEvent) error
	UpdateEventDetail(ctx context.Context, detail RequestDetail) error
	SnapshotForKey(ctx context.Context, apiKeyHash string, keyLabel string, windowDays int, active bool, now time.Time, enabled bool) (Snapshot, error)
	Analytics(ctx context.Context, period string, now time.Time, enabled bool, activeRequests []ActiveRequest) (AnalyticsSnapshot, error)
	RequestDetails(ctx context.Context, filter RequestDetailsFilter, now time.Time) (RequestDetailsSnapshot, error)
	Close() error
}

func SetRepository(repository Repository) {
	defaultStore.SetRepository(repository)
}

func (s *Store) SetRepository(repository Repository) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.repository = repository
	s.mu.Unlock()
}

func (s *Store) repositoryLocked() Repository {
	if s == nil {
		return nil
	}
	return s.repository
}

func (s *Store) persistUsageEvent(ctx context.Context, apiKey string, request RecentRequest, detail RequestDetail) {
	if s == nil {
		return
	}
	s.mu.Lock()
	repository := s.repositoryLocked()
	enabled := s.enabled.Load()
	s.mu.Unlock()
	if repository == nil || !enabled {
		return
	}

	persistCtx, cancel := context.WithTimeout(context.Background(), repositoryInsertTimeout)
	defer cancel()
	if err := repository.InsertEvent(persistCtx, UsageEvent{
		APIKeyHash: hashAPIKey(apiKey),
		Request:    request,
		Detail:     detail,
	}); err != nil {
		log.WithError(err).Warn("usage analytics: persist event failed")
	}
}

func (s *Store) persistRequestDetailUpdates(details []RequestDetail) {
	if s == nil || len(details) == 0 {
		return
	}
	s.mu.Lock()
	repository := s.repositoryLocked()
	enabled := s.enabled.Load()
	s.mu.Unlock()
	if repository == nil || !enabled {
		return
	}

	persistCtx, cancel := context.WithTimeout(context.Background(), repositoryInsertTimeout)
	defer cancel()
	for _, detail := range details {
		if err := repository.UpdateEventDetail(persistCtx, detail); err != nil {
			log.WithError(err).Warn("usage analytics: persist request detail update failed")
		}
	}
}

func (s *Store) snapshotFromRepository(apiKey string, windowDays int, active bool, now time.Time) (Snapshot, bool) {
	if s == nil {
		return Snapshot{}, false
	}
	s.mu.Lock()
	repository := s.repositoryLocked()
	enabled := s.enabled.Load()
	s.mu.Unlock()
	if repository == nil || !enabled {
		return Snapshot{}, false
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), repositoryQueryTimeout)
	defer cancel()
	out, err := repository.SnapshotForKey(queryCtx, hashAPIKey(apiKey), MaskAPIKey(apiKey), windowDays, active, now, enabled)
	if err != nil {
		log.WithError(err).Warn("usage analytics: repository key snapshot failed; falling back to memory")
		return Snapshot{}, false
	}
	return out, true
}

func (s *Store) analyticsFromRepository(period string, now time.Time) (AnalyticsSnapshot, bool) {
	if s == nil {
		return AnalyticsSnapshot{}, false
	}
	s.mu.Lock()
	repository := s.repositoryLocked()
	enabled := s.enabled.Load()
	s.pruneActiveLocked(now)
	activeRequests := s.activeRequestsLocked(now)
	s.mu.Unlock()
	if repository == nil || !enabled {
		return AnalyticsSnapshot{}, false
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), repositoryQueryTimeout)
	defer cancel()
	out, err := repository.Analytics(queryCtx, period, now, enabled, activeRequests)
	if err != nil {
		log.WithError(err).Warn("usage analytics: repository analytics failed; falling back to memory")
		return AnalyticsSnapshot{}, false
	}
	return out, true
}

func (s *Store) requestDetailsFromRepository(filter RequestDetailsFilter, now time.Time) (RequestDetailsSnapshot, bool) {
	if s == nil {
		return RequestDetailsSnapshot{}, false
	}
	s.mu.Lock()
	repository := s.repositoryLocked()
	enabled := s.enabled.Load()
	s.mu.Unlock()
	if repository == nil || !enabled {
		return RequestDetailsSnapshot{}, false
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), repositoryQueryTimeout)
	defer cancel()
	out, err := repository.RequestDetails(queryCtx, filter, now)
	if err != nil {
		log.WithError(err).Warn("usage analytics: repository request details failed; falling back to memory")
		return RequestDetailsSnapshot{}, false
	}
	return out, true
}

func hashAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}
