package dal

import (
	"context"
	"errors"
	"testing"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"
)

func setupQuotaTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

func seedQuota(t *testing.T, db *gorm.DB, ownerType int32, ownerID, total, used int64) *models.StorageQuota {
	t.Helper()
	q := &models.StorageQuota{
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TotalBytes: total,
		UsedBytes:  used,
	}
	if err := db.Create(q).Error; err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	return q
}

func TestIncrementQuotaUsed_Success(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 100)

	if err := IncrementQuotaUsed(ctx, db, 1, 100, 200); err != nil {
		t.Fatalf("IncrementQuotaUsed: %v", err)
	}
	q, err := GetQuotaByOwner(ctx, db, 1, 100)
	if err != nil {
		t.Fatalf("GetQuotaByOwner: %v", err)
	}
	if q.UsedBytes != 300 {
		t.Errorf("UsedBytes: got %d want 300", q.UsedBytes)
	}
}

func TestIncrementQuotaUsed_ExceedsTotal(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 900)

	err := IncrementQuotaUsed(ctx, db, 1, 100, 200)
	if err == nil {
		t.Fatal("want ErrQuotaExceeded, got nil")
	}
	if !errors.Is(err, xcodes.ErrQuotaExceeded.New()) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}

	q, _ := GetQuotaByOwner(ctx, db, 1, 100)
	if q.UsedBytes != 900 {
		t.Errorf("UsedBytes should be unchanged: got %d want 900", q.UsedBytes)
	}
}

func TestIncrementQuotaUsed_UnknownOwner(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()

	err := IncrementQuotaUsed(ctx, db, 1, 999, 100)
	if !errors.Is(err, xcodes.ErrQuotaExceeded.New()) {
		t.Fatalf("want ErrQuotaExceeded (rowsAffected=0), got %v", err)
	}
}

func TestAddQuota_SuccessAndRefund(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 500)

	if err := AddQuota(ctx, db, 1, 100, 500); err != nil {
		t.Fatalf("AddQuota +500: %v", err)
	}
	q, _ := GetQuotaByOwner(ctx, db, 1, 100)
	if q.TotalBytes != 1500 {
		t.Errorf("after +500: TotalBytes got %d want 1500", q.TotalBytes)
	}

	if err := AddQuota(ctx, db, 1, 100, -800); err != nil {
		t.Fatalf("AddQuota -800: %v", err)
	}
	q, _ = GetQuotaByOwner(ctx, db, 1, 100)
	if q.TotalBytes != 700 {
		t.Errorf("after -800: TotalBytes got %d want 700", q.TotalBytes)
	}
}

func TestAddQuota_RefundTooLarge(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 500)

	err := AddQuota(ctx, db, 1, 100, -1500)
	if !errors.Is(err, xcodes.ErrQuotaInsufficientTotal.New()) {
		t.Fatalf("want ErrQuotaInsufficientTotal, got %v", err)
	}

	q, _ := GetQuotaByOwner(ctx, db, 1, 100)
	if q.TotalBytes != 1000 {
		t.Errorf("TotalBytes should be unchanged: got %d want 1000", q.TotalBytes)
	}
}

// --- happy-path coverage for the remaining quota.go functions ---

// TestGetQuotaByOwner verifies the row fetch + ErrQuotaNotFound for missing.
func TestGetQuotaByOwner(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 200)

	got, err := GetQuotaByOwner(ctx, db, 1, 100)
	if err != nil {
		t.Fatalf("GetQuotaByOwner: %v", err)
	}
	if got.TotalBytes != 1000 || got.UsedBytes != 200 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if _, err := GetQuotaByOwner(ctx, db, 1, 999); err == nil ||
		!errors.Is(err, xcodes.ErrQuotaNotFound.New()) {
		t.Fatalf("expected ErrQuotaNotFound, got %v", err)
	}
}

// TestCreateQuotaIfNotExist verifies first call inserts and second call dedups
// (returns existing row).
func TestCreateQuotaIfNotExist(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()

	q, err := CreateQuotaIfNotExist(ctx, db, 1, 1, 100, 1000)
	if err != nil {
		t.Fatalf("first CreateQuotaIfNotExist: %v", err)
	}
	if q.TotalBytes != 1000 {
		t.Fatalf("TotalBytes: got %d want 1000", q.TotalBytes)
	}
	firstID := q.ID

	// Second call with same owner (different snowflake id) dedups.
	q2, err := CreateQuotaIfNotExist(ctx, db, 2, 1, 100, 5000)
	if err != nil {
		t.Fatalf("second CreateQuotaIfNotExist: %v", err)
	}
	if q2.ID != firstID {
		t.Fatalf("dedup should return existing ID %d, got %d", firstID, q2.ID)
	}
	if q2.TotalBytes != 1000 {
		t.Fatalf("dedup should preserve original TotalBytes 1000, got %d", q2.TotalBytes)
	}
}

// TestDecrementQuotaUsed verifies decrement + ErrQuotaInsufficientUsed when
// decrement would go negative.
func TestDecrementQuotaUsed(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 500)

	if err := DecrementQuotaUsed(ctx, db, 1, 100, 300); err != nil {
		t.Fatalf("DecrementQuotaUsed 300: %v", err)
	}
	q, _ := GetQuotaByOwner(ctx, db, 1, 100)
	if q.UsedBytes != 200 {
		t.Fatalf("after decr 300: want 200, got %d", q.UsedBytes)
	}

	// Decrement by more than current used → guard fails.
	if err := DecrementQuotaUsed(ctx, db, 1, 100, 500); err == nil ||
		!errors.Is(err, xcodes.ErrQuotaInsufficientUsed.New()) {
		t.Fatalf("expected ErrQuotaInsufficientUsed, got %v", err)
	}
}

// TestSetQuota verifies total bytes update + ErrQuotaNotActive for missing owner.
func TestSetQuota(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 0)

	if err := SetQuota(ctx, db, 1, 100, 5000); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	q, _ := GetQuotaByOwner(ctx, db, 1, 100)
	if q.TotalBytes != 5000 {
		t.Fatalf("TotalBytes: got %d want 5000", q.TotalBytes)
	}

	// Missing owner → ErrQuotaNotActive.
	if err := SetQuota(ctx, db, 1, 999, 1000); err == nil ||
		!errors.Is(err, xcodes.ErrQuotaNotActive.New()) {
		t.Fatalf("expected ErrQuotaNotActive, got %v", err)
	}
}

// TestDeleteQuotaByOwner verifies the soft-delete leaves no retrievable row.
func TestDeleteQuotaByOwner(t *testing.T) {
	db := setupQuotaTestDB(t)
	ctx := context.Background()
	seedQuota(t, db, 1, 100, 1000, 0)

	if err := DeleteQuotaByOwner(ctx, db, 1, 100); err != nil {
		t.Fatalf("DeleteQuotaByOwner: %v", err)
	}
	if _, err := GetQuotaByOwner(ctx, db, 1, 100); err == nil ||
		!errors.Is(err, xcodes.ErrQuotaNotFound.New()) {
		t.Fatalf("expected ErrQuotaNotFound after delete, got %v", err)
	}
}
