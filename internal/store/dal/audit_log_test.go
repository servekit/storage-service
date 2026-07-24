package dal

import (
	"context"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/storage-service/internal/store/models"
)

func setupAuditLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

func seedAuditLog(t *testing.T, db *gorm.DB, ownerType int32, ownerID int64, action int32) models.StorageAuditLog {
	t.Helper()
	log := models.StorageAuditLog{
		Action:     action,
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: 1,
		TargetID:   1,
		Status:     1,
	}
	ctx := context.Background()
	if err := CreateAuditLog(ctx, db, &log); err != nil {
		t.Fatalf("seed CreateAuditLog: %v", err)
	}
	if log.ID == 0 {
		t.Fatal("expected ID to be set after CreateAuditLog")
	}
	return log
}

// TestCreateAuditLog inserts a single audit log and verifies the row round-trips.
func TestCreateAuditLog(t *testing.T) {
	db := setupAuditLogTestDB(t)
	ctx := context.Background()

	log := models.StorageAuditLog{
		Action:     1,
		OwnerType:  1,
		OwnerID:    100,
		TargetType: 2,
		TargetID:   999,
		Status:     1,
		RequestID:  "req-abc",
	}
	if err := CreateAuditLog(ctx, db, &log); err != nil {
		t.Fatalf("CreateAuditLog: %v", err)
	}
	if log.ID == 0 {
		t.Fatal("expected ID to be set after CreateAuditLog")
	}

	var got models.StorageAuditLog
	if err := db.First(&got, log.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Action != log.Action || got.OwnerID != log.OwnerID || got.RequestID != "req-abc" {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
}

// TestListAuditLogsByOwner verifies owner-scoped listing returns only that
// owner's rows and the count matches.
func TestListAuditLogsByOwner(t *testing.T) {
	db := setupAuditLogTestDB(t)
	ctx := context.Background()

	seedAuditLog(t, db, 1, 100, 1)
	seedAuditLog(t, db, 1, 100, 2)
	seedAuditLog(t, db, 1, 200, 1) // different owner — must be excluded

	logs, total, err := ListAuditLogsByOwner(ctx, db, 1, 100, AuditLogFilter{})
	if err != nil {
		t.Fatalf("ListAuditLogsByOwner: %v", err)
	}
	if total != 2 {
		t.Fatalf("want total=2, got %d", total)
	}
	if len(logs) != 2 {
		t.Fatalf("want 2 logs, got %d", len(logs))
	}
	for _, l := range logs {
		if l.OwnerID != 100 {
			t.Errorf("got log for owner %d, want 100", l.OwnerID)
		}
	}
}

// TestListAllAuditLogs verifies admin listing returns every row regardless of
// owner and that the optional filters narrow correctly.
func TestListAllAuditLogs(t *testing.T) {
	db := setupAuditLogTestDB(t)
	ctx := context.Background()

	seedAuditLog(t, db, 1, 100, 1)
	seedAuditLog(t, db, 1, 200, 2)
	seedAuditLog(t, db, 2, 300, 1)

	// No filter — all three rows returned.
	logs, total, err := ListAllAuditLogs(ctx, db, AuditLogFilter{})
	if err != nil {
		t.Fatalf("ListAllAuditLogs: %v", err)
	}
	if total != 3 || len(logs) != 3 {
		t.Fatalf("want total=3 len=3, got total=%d len=%d", total, len(logs))
	}

	// Filter by owner — narrows to one row.
	logs, total, err = ListAllAuditLogs(ctx, db, AuditLogFilter{OwnerType: 1, OwnerID: 100})
	if err != nil {
		t.Fatalf("ListAllAuditLogs (filtered): %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("want total=1 len=1 after filter, got total=%d len=%d", total, len(logs))
	}

	// Filter by time window in the future — zero rows.
	future := time.Now().Add(time.Hour)
	logs, total, err = ListAllAuditLogs(ctx, db, AuditLogFilter{StartTime: future})
	if err != nil {
		t.Fatalf("ListAllAuditLogs (future): %v", err)
	}
	if total != 0 || len(logs) != 0 {
		t.Fatalf("want total=0 len=0 after future filter, got total=%d len=%d", total, len(logs))
	}
}
