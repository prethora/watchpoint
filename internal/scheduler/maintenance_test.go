package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ============================================================
// Shared Test Logger
// ============================================================

func maintenanceTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ============================================================
// Mock: CleanupDB
// ============================================================

type mockCleanupDB struct {
	mu sync.Mutex

	// PurgeSoftDeletedOrgs
	softDeletedOrgIDs []string
	listSoftDeleteErr error
	hardDeleteErr     error
	hardDeletedOrgs   []string

	// PurgeExpiredInvites
	expiredInvitesCount int
	deleteInvitesErr    error

	// PurgeArchivedWatchPoints
	archivedWPIDs       []string
	listArchivedErr     error
	hardDeleteWPCount   int
	hardDeleteWPErr     error
	hardDeletedWPIDs    []string

	// PurgeNotifications
	deleteNotifCount int
	deleteNotifErr   error

	// ArchiveAuditLogs
	auditEntries      []AuditLogEntry
	listAuditErr      error
	deleteAuditCount  int
	deleteAuditErr    error
	deletedAuditIDs   []int64

	// PurgeExpiredIdempotencyKeys
	deleteIdempotencyCount int
	deleteIdempotencyErr   error

	// PurgeSecurityEvents
	deleteSecurityCount int
	deleteSecurityErr   error
}

func (m *mockCleanupDB) ListSoftDeletedOrgIDs(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listSoftDeleteErr != nil {
		return nil, m.listSoftDeleteErr
	}
	return m.softDeletedOrgIDs, nil
}

func (m *mockCleanupDB) HardDeleteOrg(_ context.Context, orgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hardDeleteErr != nil {
		return m.hardDeleteErr
	}
	m.hardDeletedOrgs = append(m.hardDeletedOrgs, orgID)
	return nil
}

func (m *mockCleanupDB) DeleteExpiredInvites(_ context.Context, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteInvitesErr != nil {
		return 0, m.deleteInvitesErr
	}
	return m.expiredInvitesCount, nil
}

func (m *mockCleanupDB) ListArchivedWatchPointIDs(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listArchivedErr != nil {
		return nil, m.listArchivedErr
	}
	return m.archivedWPIDs, nil
}

func (m *mockCleanupDB) HardDeleteWatchPoints(_ context.Context, ids []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hardDeleteWPErr != nil {
		return 0, m.hardDeleteWPErr
	}
	m.hardDeletedWPIDs = append(m.hardDeletedWPIDs, ids...)
	return len(ids), nil
}

func (m *mockCleanupDB) DeleteNotificationsBefore(_ context.Context, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteNotifErr != nil {
		return 0, m.deleteNotifErr
	}
	return m.deleteNotifCount, nil
}

func (m *mockCleanupDB) ListAuditLogsOlderThan(_ context.Context, _ time.Time, _ int) ([]AuditLogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listAuditErr != nil {
		return nil, m.listAuditErr
	}
	result := m.auditEntries
	m.auditEntries = nil // Simulate consumed batch
	return result, nil
}

func (m *mockCleanupDB) DeleteAuditLogsByIDs(_ context.Context, ids []int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteAuditErr != nil {
		return 0, m.deleteAuditErr
	}
	m.deletedAuditIDs = append(m.deletedAuditIDs, ids...)
	return len(ids), nil
}

func (m *mockCleanupDB) DeleteExpiredIdempotencyKeys(_ context.Context, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteIdempotencyErr != nil {
		return 0, m.deleteIdempotencyErr
	}
	return m.deleteIdempotencyCount, nil
}

func (m *mockCleanupDB) DeleteSecurityEventsBefore(_ context.Context, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteSecurityErr != nil {
		return 0, m.deleteSecurityErr
	}
	return m.deleteSecurityCount, nil
}

// ============================================================
// Mock: AuditLogArchiver
// ============================================================

type mockAuditArchiver struct {
	mu          sync.Mutex
	uploadedKeys []string
	uploadedData [][]byte
	uploadErr   error
}

func (m *mockAuditArchiver) UploadArchive(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uploadErr != nil {
		return m.uploadErr
	}
	m.uploadedKeys = append(m.uploadedKeys, key)
	m.uploadedData = append(m.uploadedData, data)
	return nil
}

// ============================================================
// Mock: ArchiverDB
// ============================================================

type mockArchiverDB struct {
	mu       sync.Mutex
	archived []ArchivedWatchPoint
	err      error
}

func (m *mockArchiverDB) ArchiveExpiredWatchPoints(_ context.Context, _ time.Time, _ time.Duration) ([]ArchivedWatchPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.archived, nil
}

// ============================================================
// Mock: ArchiverSQSPublisher
// ============================================================

type mockArchiverPublisher struct {
	mu        sync.Mutex
	published []types.EvalMessage
	err       error
	failOnIDs map[string]bool // WatchPoint IDs that should fail to publish
}

func (m *mockArchiverPublisher) PublishEvalMessage(_ context.Context, msg types.EvalMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOnIDs != nil && len(msg.SpecificWatchPointIDs) > 0 && m.failOnIDs[msg.SpecificWatchPointIDs[0]] {
		return fmt.Errorf("SQS publish error for %s", msg.SpecificWatchPointIDs[0])
	}
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, msg)
	return nil
}

// ============================================================
// Mock: TierTransitionDB
// ============================================================

type mockTierTransitionDB struct {
	mu   sync.Mutex
	runs map[types.ForecastType][]ExpiredForecastRun
	listErr  map[types.ForecastType]error
	markErr  error
	markedDeleted []string
}

func (m *mockTierTransitionDB) ListExpiredForecastRuns(_ context.Context, model types.ForecastType, _ time.Time, _ int) ([]ExpiredForecastRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		if err, ok := m.listErr[model]; ok {
			return nil, err
		}
	}
	if m.runs == nil {
		return nil, nil
	}
	return m.runs[model], nil
}

func (m *mockTierTransitionDB) MarkForecastRunDeleted(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErr != nil {
		return m.markErr
	}
	m.markedDeleted = append(m.markedDeleted, id)
	return nil
}

// ============================================================
// Mock: S3Deleter
// ============================================================

type mockS3Deleter struct {
	mu      sync.Mutex
	deleted []string
	err     error
	failOn  map[string]error // path -> error, for selective failures
}

func (m *mockS3Deleter) DeleteObjects(_ context.Context, storagePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != nil {
		if err, ok := m.failOn[storagePath]; ok {
			return err
		}
	}
	if m.err != nil {
		return m.err
	}
	m.deleted = append(m.deleted, storagePath)
	return nil
}

// ============================================================
// Mock: DeferredNotificationDB
// ============================================================

type mockDeferredDB struct {
	mu         sync.Mutex
	deliveries []DeferredDelivery
	listErr    error

	resetCalls []string
	resetErr   error

	// Track which IDs were reset, and return false for specific ones
	alreadyTransitioned map[string]bool
}

func (m *mockDeferredDB) ListDeferredDeliveries(_ context.Context, _ time.Time, _ int) ([]DeferredDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.deliveries, nil
}

func (m *mockDeferredDB) ResetDeliveryToPending(_ context.Context, deliveryID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetCalls = append(m.resetCalls, deliveryID)
	if m.resetErr != nil {
		return false, m.resetErr
	}
	if m.alreadyTransitioned != nil && m.alreadyTransitioned[deliveryID] {
		return false, nil
	}
	return true, nil
}

// ============================================================
// Mock: DeferredNotificationPublisher
// ============================================================

type mockDeferredPublisher struct {
	mu        sync.Mutex
	published []map[string]any
	err       error
	failOnIdx int // -1 means no selective failure
	callCount int
}

func (m *mockDeferredPublisher) PublishNotification(_ context.Context, payload map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount++
	if m.failOnIdx >= 0 && m.callCount == m.failOnIdx {
		return fmt.Errorf("SQS unavailable")
	}
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, payload)
	return nil
}

// ============================================================
// CleanupService Tests
// ============================================================

func TestPurgeSoftDeletedOrgs_Success(t *testing.T) {
	db := &mockCleanupDB{
		softDeletedOrgIDs: []string{"org_1", "org_2", "org_3"},
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())
	now := time.Date(2026, 2, 6, 2, 0, 0, 0, time.UTC)

	count, err := svc.PurgeSoftDeletedOrgs(ctx(), now, 30*24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 deleted, got %d", count)
	}
	if len(db.hardDeletedOrgs) != 3 {
		t.Errorf("expected 3 hard deletes, got %d", len(db.hardDeletedOrgs))
	}
}

func TestPurgeSoftDeletedOrgs_NoCandidates(t *testing.T) {
	db := &mockCleanupDB{
		softDeletedOrgIDs: []string{},
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeSoftDeletedOrgs(ctx(), time.Now(), 30*24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 deleted, got %d", count)
	}
}

func TestPurgeSoftDeletedOrgs_ListError(t *testing.T) {
	db := &mockCleanupDB{
		listSoftDeleteErr: errors.New("db down"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeSoftDeletedOrgs(ctx(), time.Now(), 30*24*time.Hour, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPurgeSoftDeletedOrgs_PartialFailure(t *testing.T) {
	// Simulates one org failing to delete while others succeed.
	// We use a custom mock that fails on a specific org.
	db := &mockPartialDeleteDB{
		orgIDs:  []string{"org_1", "org_fail", "org_3"},
		failOrg: "org_fail",
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeSoftDeletedOrgs(ctx(), time.Now(), 30*24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// org_1 and org_3 succeed, org_fail is skipped
	if count != 2 {
		t.Errorf("expected 2 deleted (1 failed), got %d", count)
	}
}

func TestPurgeExpiredInvites_Success(t *testing.T) {
	db := &mockCleanupDB{
		expiredInvitesCount: 5,
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeExpiredInvites(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5, got %d", count)
	}
}

func TestPurgeExpiredInvites_Error(t *testing.T) {
	db := &mockCleanupDB{
		deleteInvitesErr: errors.New("constraint violation"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeExpiredInvites(ctx(), time.Now())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPurgeArchivedWatchPoints_Success(t *testing.T) {
	db := &mockCleanupDB{
		archivedWPIDs: []string{"wp_1", "wp_2"},
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeArchivedWatchPoints(ctx(), time.Now(), 90*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
	if len(db.hardDeletedWPIDs) != 2 {
		t.Errorf("expected 2 IDs passed to delete, got %d", len(db.hardDeletedWPIDs))
	}
}

func TestPurgeArchivedWatchPoints_NoCandidates(t *testing.T) {
	db := &mockCleanupDB{
		archivedWPIDs: []string{},
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeArchivedWatchPoints(ctx(), time.Now(), 90*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPurgeArchivedWatchPoints_DeleteError(t *testing.T) {
	db := &mockCleanupDB{
		archivedWPIDs:   []string{"wp_1"},
		hardDeleteWPErr: errors.New("FK violation"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeArchivedWatchPoints(ctx(), time.Now(), 90*24*time.Hour, 1000)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPurgeNotifications_Success(t *testing.T) {
	db := &mockCleanupDB{
		deleteNotifCount: 42,
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeNotifications(ctx(), 90*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestPurgeNotifications_Error(t *testing.T) {
	db := &mockCleanupDB{
		deleteNotifErr: errors.New("timeout"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeNotifications(ctx(), 90*24*time.Hour)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestArchiveAuditLogs_Success(t *testing.T) {
	orgID := "org_123"
	entries := []AuditLogEntry{
		{ID: 1, OrganizationID: &orgID, Action: "create", ResourceType: "watchpoint", ResourceID: "wp_1", CreatedAt: time.Now().Add(-3 * 365 * 24 * time.Hour)},
		{ID: 2, OrganizationID: &orgID, Action: "delete", ResourceType: "watchpoint", ResourceID: "wp_2", CreatedAt: time.Now().Add(-3 * 365 * 24 * time.Hour)},
	}
	db := &mockCleanupDB{
		auditEntries: entries,
	}
	archiver := &mockAuditArchiver{}
	svc := NewCleanupService(db, archiver, maintenanceTestLogger())

	count, err := svc.ArchiveAuditLogs(ctx(), 2*365*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 archived, got %d", count)
	}
	if len(archiver.uploadedKeys) != 1 {
		t.Errorf("expected 1 upload, got %d", len(archiver.uploadedKeys))
	}
	if len(db.deletedAuditIDs) != 2 {
		t.Errorf("expected 2 audit IDs deleted, got %d", len(db.deletedAuditIDs))
	}
}

func TestArchiveAuditLogs_NoArchiver(t *testing.T) {
	db := &mockCleanupDB{}
	svc := NewCleanupService(db, nil, maintenanceTestLogger()) // nil archiver

	count, err := svc.ArchiveAuditLogs(ctx(), 2*365*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 (skipped), got %d", count)
	}
}

func TestArchiveAuditLogs_UploadError(t *testing.T) {
	orgID := "org_123"
	db := &mockCleanupDB{
		auditEntries: []AuditLogEntry{
			{ID: 1, OrganizationID: &orgID, Action: "create", ResourceType: "wp", ResourceID: "wp_1", CreatedAt: time.Now()},
		},
	}
	archiver := &mockAuditArchiver{uploadErr: errors.New("S3 error")}
	svc := NewCleanupService(db, archiver, maintenanceTestLogger())

	_, err := svc.ArchiveAuditLogs(ctx(), 2*365*24*time.Hour, 1000)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestArchiveAuditLogs_DeleteError(t *testing.T) {
	orgID := "org_123"
	db := &mockCleanupDB{
		auditEntries: []AuditLogEntry{
			{ID: 1, OrganizationID: &orgID, Action: "create", ResourceType: "wp", ResourceID: "wp_1", CreatedAt: time.Now()},
		},
		deleteAuditErr: errors.New("delete failed"),
	}
	archiver := &mockAuditArchiver{}
	svc := NewCleanupService(db, archiver, maintenanceTestLogger())

	_, err := svc.ArchiveAuditLogs(ctx(), 2*365*24*time.Hour, 1000)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestArchiveAuditLogs_NoEntries(t *testing.T) {
	db := &mockCleanupDB{
		auditEntries: []AuditLogEntry{},
	}
	archiver := &mockAuditArchiver{}
	svc := NewCleanupService(db, archiver, maintenanceTestLogger())

	count, err := svc.ArchiveAuditLogs(ctx(), 2*365*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
	if len(archiver.uploadedKeys) != 0 {
		t.Errorf("expected 0 uploads, got %d", len(archiver.uploadedKeys))
	}
}

func TestPurgeExpiredIdempotencyKeys_Success(t *testing.T) {
	db := &mockCleanupDB{
		deleteIdempotencyCount: 100,
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeExpiredIdempotencyKeys(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 100 {
		t.Errorf("expected 100, got %d", count)
	}
}

func TestPurgeExpiredIdempotencyKeys_Error(t *testing.T) {
	db := &mockCleanupDB{
		deleteIdempotencyErr: errors.New("db error"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeExpiredIdempotencyKeys(ctx(), time.Now())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPurgeSecurityEvents_Success(t *testing.T) {
	db := &mockCleanupDB{
		deleteSecurityCount: 50,
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	count, err := svc.PurgeSecurityEvents(ctx(), time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 50 {
		t.Errorf("expected 50, got %d", count)
	}
}

func TestPurgeSecurityEvents_Error(t *testing.T) {
	db := &mockCleanupDB{
		deleteSecurityErr: errors.New("timeout"),
	}
	svc := NewCleanupService(db, nil, maintenanceTestLogger())

	_, err := svc.PurgeSecurityEvents(ctx(), time.Now(), 7*24*time.Hour)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ============================================================
// ArchiverService Tests
// ============================================================

func TestArchiveExpired_Success(t *testing.T) {
	db := &mockArchiverDB{
		archived: []ArchivedWatchPoint{
			{ID: "wp_1", TileID: "tile_a", OrganizationID: "org_1", Name: "Storm Watch"},
			{ID: "wp_2", TileID: "tile_b", OrganizationID: "org_1", Name: "Event Monitor"},
		},
	}
	pub := &mockArchiverPublisher{}
	svc := NewArchiverService(db, pub, maintenanceTestLogger())
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	count, err := svc.ArchiveExpired(ctx(), now, 1*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 archived, got %d", count)
	}

	// Verify EvalMessages were published.
	if len(pub.published) != 2 {
		t.Fatalf("expected 2 published messages, got %d", len(pub.published))
	}

	// Verify first message content.
	msg1 := pub.published[0]
	if msg1.Action != types.EvalActionGenerateSummary {
		t.Errorf("expected action %q, got %q", types.EvalActionGenerateSummary, msg1.Action)
	}
	if msg1.TileID != "tile_a" {
		t.Errorf("expected tile_id %q, got %q", "tile_a", msg1.TileID)
	}
	if len(msg1.SpecificWatchPointIDs) != 1 || msg1.SpecificWatchPointIDs[0] != "wp_1" {
		t.Errorf("expected specific_watchpoint_ids [wp_1], got %v", msg1.SpecificWatchPointIDs)
	}
}

func TestArchiveExpired_NoCandidates(t *testing.T) {
	db := &mockArchiverDB{
		archived: []ArchivedWatchPoint{},
	}
	pub := &mockArchiverPublisher{}
	svc := NewArchiverService(db, pub, maintenanceTestLogger())

	count, err := svc.ArchiveExpired(ctx(), time.Now(), 1*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
	if len(pub.published) != 0 {
		t.Errorf("expected 0 published, got %d", len(pub.published))
	}
}

func TestArchiveExpired_DBError(t *testing.T) {
	db := &mockArchiverDB{err: errors.New("db error")}
	pub := &mockArchiverPublisher{}
	svc := NewArchiverService(db, pub, maintenanceTestLogger())

	_, err := svc.ArchiveExpired(ctx(), time.Now(), 1*time.Hour)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestArchiveExpired_PublishPartialFailure(t *testing.T) {
	db := &mockArchiverDB{
		archived: []ArchivedWatchPoint{
			{ID: "wp_1", TileID: "tile_a", OrganizationID: "org_1", Name: "Watch 1"},
			{ID: "wp_fail", TileID: "tile_b", OrganizationID: "org_1", Name: "Watch Fail"},
			{ID: "wp_3", TileID: "tile_c", OrganizationID: "org_1", Name: "Watch 3"},
		},
	}
	pub := &mockArchiverPublisher{
		failOnIDs: map[string]bool{"wp_fail": true},
	}
	svc := NewArchiverService(db, pub, maintenanceTestLogger())

	count, err := svc.ArchiveExpired(ctx(), time.Now(), 1*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All 3 were archived in DB (the UPDATE already happened), but only 2 SQS messages.
	if count != 3 {
		t.Errorf("expected 3 archived (DB committed), got %d", count)
	}
	if len(pub.published) != 2 {
		t.Errorf("expected 2 published (1 failed), got %d", len(pub.published))
	}
}

// ============================================================
// TierTransitionService Tests
// ============================================================

func TestEnforceRetention_Success(t *testing.T) {
	now := time.Date(2026, 2, 6, 4, 0, 0, 0, time.UTC)
	db := &mockTierTransitionDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{
			types.ForecastNowcast: {
				{ID: "run_nc_1", StoragePath: "s3://bucket/nowcast/2026-01-25/"},
				{ID: "run_nc_2", StoragePath: "s3://bucket/nowcast/2026-01-20/"},
			},
			types.ForecastMediumRange: {
				{ID: "run_mr_1", StoragePath: "s3://bucket/medium_range/2025-11-01/"},
			},
		},
	}
	s3 := &mockS3Deleter{}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 deleted, got %d", count)
	}

	// Verify S3 deletes.
	if len(s3.deleted) != 3 {
		t.Fatalf("expected 3 S3 delete calls, got %d", len(s3.deleted))
	}

	// Verify DB marks.
	if len(db.markedDeleted) != 3 {
		t.Fatalf("expected 3 DB mark calls, got %d", len(db.markedDeleted))
	}
}

func TestEnforceRetention_NoCandidates(t *testing.T) {
	db := &mockTierTransitionDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{},
	}
	s3 := &mockS3Deleter{}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
	if len(s3.deleted) != 0 {
		t.Errorf("expected 0 S3 deletes, got %d", len(s3.deleted))
	}
}

func TestEnforceRetention_S3DeleteFailure_SkipsRun(t *testing.T) {
	db := &mockTierTransitionDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{
			types.ForecastNowcast: {
				{ID: "run_ok", StoragePath: "s3://bucket/nowcast/ok/"},
				{ID: "run_fail", StoragePath: "s3://bucket/nowcast/fail/"},
			},
		},
	}
	s3 := &mockS3Deleter{
		failOn: map[string]error{
			"s3://bucket/nowcast/fail/": errors.New("access denied"),
		},
	}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only "run_ok" should succeed.
	if count != 1 {
		t.Errorf("expected 1 (1 failed), got %d", count)
	}
	// run_fail should NOT be marked as deleted in DB.
	if len(db.markedDeleted) != 1 {
		t.Fatalf("expected 1 DB mark, got %d", len(db.markedDeleted))
	}
	if db.markedDeleted[0] != "run_ok" {
		t.Errorf("expected run_ok to be marked, got %s", db.markedDeleted[0])
	}
}

func TestEnforceRetention_DBMarkError_ContinuesWithOthers(t *testing.T) {
	db := &mockTierTransitionMarkFailDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{
			types.ForecastNowcast: {
				{ID: "run_1", StoragePath: "s3://bucket/nowcast/1/"},
				{ID: "run_fail_mark", StoragePath: "s3://bucket/nowcast/fail_mark/"},
				{ID: "run_3", StoragePath: "s3://bucket/nowcast/3/"},
			},
		},
		failMarkID: "run_fail_mark",
	}
	s3 := &mockS3Deleter{}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 (1 mark failed), got %d", count)
	}
}

func TestEnforceRetention_EmptyStoragePath_SkipsS3Delete(t *testing.T) {
	db := &mockTierTransitionDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{
			types.ForecastNowcast: {
				{ID: "run_empty", StoragePath: ""},
			},
		},
	}
	s3 := &mockS3Deleter{}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}
	// S3 should NOT be called for empty storage_path.
	if len(s3.deleted) != 0 {
		t.Errorf("expected 0 S3 deletes (empty path), got %d", len(s3.deleted))
	}
	// DB should still mark as deleted.
	if len(db.markedDeleted) != 1 {
		t.Errorf("expected 1 DB mark, got %d", len(db.markedDeleted))
	}
}

func TestEnforceRetention_NowcastFailure_MediumRangeContinues(t *testing.T) {
	db := &mockTierTransitionDB{
		runs: map[types.ForecastType][]ExpiredForecastRun{
			types.ForecastMediumRange: {
				{ID: "run_mr_1", StoragePath: "s3://bucket/medium_range/old/"},
			},
		},
		listErr: map[types.ForecastType]error{
			types.ForecastNowcast: errors.New("nowcast list error"),
		},
	}
	s3 := &mockS3Deleter{}
	svc := NewTierTransitionService(db, s3, maintenanceTestLogger())

	count, err := svc.EnforceRetention(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Medium-range should still succeed.
	if count != 1 {
		t.Errorf("expected 1 (nowcast failed, medium-range ok), got %d", count)
	}
}

func TestEnforceRetention_RetentionConstants(t *testing.T) {
	// Verify the retention constants match the architecture spec.
	if NowcastRetention != 7*24*time.Hour {
		t.Errorf("NowcastRetention = %v, want 7 days", NowcastRetention)
	}
	if MediumRangeRetention != 90*24*time.Hour {
		t.Errorf("MediumRangeRetention = %v, want 90 days", MediumRangeRetention)
	}
}

// ============================================================
// DeferredNotificationService Tests
// ============================================================

func TestRequeueDeferredNotifications_Success(t *testing.T) {
	db := &mockDeferredDB{
		deliveries: []DeferredDelivery{
			{DeliveryID: "del_1", NotificationID: "notif_1", Payload: map[string]any{"type": "alert"}},
			{DeliveryID: "del_2", NotificationID: "notif_2", Payload: map[string]any{"type": "warning"}},
		},
	}
	pub := &mockDeferredPublisher{failOnIdx: -1}
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	count, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 requeued, got %d", count)
	}

	// Verify reset was called for both.
	if len(db.resetCalls) != 2 {
		t.Fatalf("expected 2 reset calls, got %d", len(db.resetCalls))
	}

	// Verify payloads were published.
	if len(pub.published) != 2 {
		t.Fatalf("expected 2 published, got %d", len(pub.published))
	}
	if pub.published[0]["type"] != "alert" {
		t.Errorf("expected payload type 'alert', got %v", pub.published[0]["type"])
	}
}

func TestRequeueDeferredNotifications_NoCandidates(t *testing.T) {
	db := &mockDeferredDB{
		deliveries: []DeferredDelivery{},
	}
	pub := &mockDeferredPublisher{failOnIdx: -1}
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	count, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestRequeueDeferredNotifications_ListError(t *testing.T) {
	db := &mockDeferredDB{listErr: errors.New("db error")}
	pub := &mockDeferredPublisher{failOnIdx: -1}
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	_, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRequeueDeferredNotifications_AlreadyTransitioned(t *testing.T) {
	// Simulates idempotency: delivery was already transitioned by another process.
	db := &mockDeferredDB{
		deliveries: []DeferredDelivery{
			{DeliveryID: "del_1", NotificationID: "notif_1", Payload: map[string]any{"type": "alert"}},
		},
		alreadyTransitioned: map[string]bool{"del_1": true},
	}
	pub := &mockDeferredPublisher{failOnIdx: -1}
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	count, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Not counted as requeued since it was already done.
	if count != 0 {
		t.Errorf("expected 0 (already transitioned), got %d", count)
	}
	// Should NOT publish to SQS since the status didn't change.
	if len(pub.published) != 0 {
		t.Errorf("expected 0 published, got %d", len(pub.published))
	}
}

func TestRequeueDeferredNotifications_ResetError_Continues(t *testing.T) {
	db := &mockDeferredDB{
		deliveries: []DeferredDelivery{
			{DeliveryID: "del_1", NotificationID: "notif_1", Payload: map[string]any{"type": "alert"}},
			{DeliveryID: "del_2", NotificationID: "notif_2", Payload: map[string]any{"type": "warning"}},
		},
		resetErr: errors.New("constraint violation"),
	}
	pub := &mockDeferredPublisher{failOnIdx: -1}
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	count, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All reset calls fail, so 0 requeued.
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestRequeueDeferredNotifications_PublishError_Continues(t *testing.T) {
	db := &mockDeferredDB{
		deliveries: []DeferredDelivery{
			{DeliveryID: "del_1", NotificationID: "notif_1", Payload: map[string]any{"type": "alert"}},
			{DeliveryID: "del_2", NotificationID: "notif_2", Payload: map[string]any{"type": "warning"}},
		},
	}
	pub := &mockDeferredPublisher{failOnIdx: 1} // Fail on first publish
	svc := NewDeferredNotificationService(db, pub, maintenanceTestLogger())

	count, err := svc.RequeueDeferredNotifications(ctx(), time.Now(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// First publish fails, second succeeds.
	if count != 1 {
		t.Errorf("expected 1 (1 publish failed), got %d", count)
	}
}

// ============================================================
// Helper Mocks
// ============================================================

// mockPartialDeleteDB is a CleanupDB mock that fails on a specific org ID.
type mockPartialDeleteDB struct {
	orgIDs       []string
	failOrg      string
	hardDeleted  []string
	mu           sync.Mutex
}

func (m *mockPartialDeleteDB) ListSoftDeletedOrgIDs(_ context.Context, _ time.Time, _ int) ([]string, error) {
	return m.orgIDs, nil
}

func (m *mockPartialDeleteDB) HardDeleteOrg(_ context.Context, orgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == m.failOrg {
		return fmt.Errorf("cascade constraint violation for %s", orgID)
	}
	m.hardDeleted = append(m.hardDeleted, orgID)
	return nil
}

func (m *mockPartialDeleteDB) DeleteExpiredInvites(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockPartialDeleteDB) ListArchivedWatchPointIDs(_ context.Context, _ time.Time, _ int) ([]string, error) {
	return nil, nil
}
func (m *mockPartialDeleteDB) HardDeleteWatchPoints(_ context.Context, _ []string) (int, error) {
	return 0, nil
}
func (m *mockPartialDeleteDB) DeleteNotificationsBefore(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockPartialDeleteDB) ListAuditLogsOlderThan(_ context.Context, _ time.Time, _ int) ([]AuditLogEntry, error) {
	return nil, nil
}
func (m *mockPartialDeleteDB) DeleteAuditLogsByIDs(_ context.Context, _ []int64) (int, error) {
	return 0, nil
}
func (m *mockPartialDeleteDB) DeleteExpiredIdempotencyKeys(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockPartialDeleteDB) DeleteSecurityEventsBefore(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

// mockTierTransitionMarkFailDB is a TierTransitionDB that fails MarkForecastRunDeleted
// for a specific run ID.
type mockTierTransitionMarkFailDB struct {
	mu            sync.Mutex
	runs          map[types.ForecastType][]ExpiredForecastRun
	failMarkID    string
	markedDeleted []string
}

func (m *mockTierTransitionMarkFailDB) ListExpiredForecastRuns(_ context.Context, model types.ForecastType, _ time.Time, _ int) ([]ExpiredForecastRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		return nil, nil
	}
	return m.runs[model], nil
}

func (m *mockTierTransitionMarkFailDB) MarkForecastRunDeleted(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == m.failMarkID {
		return fmt.Errorf("mark deleted failed for %s", id)
	}
	m.markedDeleted = append(m.markedDeleted, id)
	return nil
}

// ctx returns a background context for test brevity.
func ctx() context.Context {
	return context.Background()
}

// ============================================================
// serializeAuditLogsJSONL Test
// ============================================================

func TestSerializeAuditLogsJSONL(t *testing.T) {
	orgID := "org_test"
	entries := []AuditLogEntry{
		{ID: 1, OrganizationID: &orgID, Action: "create", ResourceType: "wp", ResourceID: "wp_1", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: 2, OrganizationID: nil, Action: "delete", ResourceType: "org", ResourceID: "org_2", CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}

	data, err := serializeAuditLogsJSONL(entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	str := string(data)
	// Should contain a newline separating two entries.
	lines := 0
	for _, c := range str {
		if c == '\n' {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("expected 1 newline between 2 entries, got %d", lines)
	}

	// Should contain both entries' IDs.
	if !containsSubstr(str, `"id":1`) {
		t.Errorf("missing id 1 in output: %s", str)
	}
	if !containsSubstr(str, `"id":2`) {
		t.Errorf("missing id 2 in output: %s", str)
	}
}

func containsSubstr(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && contains(s, substr)
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
