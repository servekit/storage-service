package dal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"
)

// setupSessionTestDB starts a Postgres testcontainer and runs AutoMigrate on
// all models so the upload_sessions table exists.
func setupSessionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

func TestUploadSessionRepo_CreateAndGetByID(t *testing.T) {
	db := setupSessionTestDB(t)
	ctx := context.Background()

	s := &models.StorageUploadSession{
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "test-bucket",
		ObjectKey:   "obj/key-1",
		MD5:         "d41d8cd98f00b204e9800998ecf8427e",
		Size:        42,
		ContentType: "text/plain",
		Filename:    "hello.txt",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID == 0 {
		t.Fatal("expected ID to be set after Create")
	}

	got, err := GetUploadSessionByID(ctx, db, s.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MD5 != s.MD5 || got.Status != int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING) {
		t.Fatalf("round-trip mismatch: got md5=%q status=%d", got.MD5, got.Status)
	}
	if got.FileID != nil {
		t.Fatalf("expected nil FileID initially, got %v", got.FileID)
	}
}

func TestUploadSessionRepo_GetByID_NotFound(t *testing.T) {
	db := setupSessionTestDB(t)

	_, err := GetUploadSessionByID(context.Background(), db, 999999)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, xcodes.ErrUploadSessionNotFound.New()) {
		t.Fatalf("expected ErrUploadSessionNotFound, got %v", err)
	}
}

func TestUploadSessionRepo_MarkConfirmed(t *testing.T) {
	db := setupSessionTestDB(t)
	ctx := context.Background()

	s := &models.StorageUploadSession{
		OwnerType:   1,
		OwnerID:     200,
		Bucket:      "b",
		ObjectKey:   "o/1",
		MD5:         "abc",
		Size:        1,
		ContentType: "text/plain",
		Filename:    "f.txt",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := MarkUploadSessionConfirmed(ctx, db, s.ID, 555); err != nil {
		t.Fatalf("MarkConfirmed: %v", err)
	}

	got, err := GetUploadSessionByID(ctx, db, s.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED) {
		t.Fatalf("expected CONFIRMED status, got %d", got.Status)
	}
	if got.FileID == nil || *got.FileID != 555 {
		t.Fatalf("expected FileID=555, got %v", got.FileID)
	}

	// Second MarkConfirmed on the same session should fail (no longer PENDING).
	if err := MarkUploadSessionConfirmed(ctx, db, s.ID, 666); !errors.Is(err, xcodes.ErrUploadSessionNotPending.New()) {
		t.Fatalf("expected ErrUploadSessionNotPending on double confirm, got %v", err)
	}
}

func TestUploadSessionRepo_MarkCancelled(t *testing.T) {
	db := setupSessionTestDB(t)
	ctx := context.Background()

	s := &models.StorageUploadSession{
		OwnerType: 1, OwnerID: 300, Bucket: "b", ObjectKey: "o/2",
		MD5: "x", Size: 1, ContentType: "t", Filename: "f", Vendor: 1,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := MarkUploadSessionCancelled(ctx, db, s.ID); err != nil {
		t.Fatalf("MarkCancelled: %v", err)
	}
	got, err := GetUploadSessionByID(ctx, db, s.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CANCELLED) {
		t.Fatalf("expected CANCELLED status, got %d", got.Status)
	}
}

func TestUploadSessionRepo_FindPendingDedup(t *testing.T) {
	db := setupSessionTestDB(t)
	ctx := context.Background()

	s := &models.StorageUploadSession{
		OwnerType: 1, OwnerID: 400, Bucket: "b", ObjectKey: "o/3",
		MD5: "dedup-md5", Size: 99, ContentType: "t", Filename: "f", Vendor: 1,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	found, ok, err := FindPendingUploadSessionDedup(ctx, db, 1, 400, "dedup-md5", 99)
	if err != nil {
		t.Fatalf("FindPendingDedup: %v", err)
	}
	if !ok || found == nil {
		t.Fatal("expected to find a pending session")
	}
	if found.ID != s.ID {
		t.Fatalf("expected ID=%d, got %d", s.ID, found.ID)
	}

	// Mismatch on size should return no match.
	_, ok, err = FindPendingUploadSessionDedup(ctx, db, 1, 400, "dedup-md5", 100)
	if err != nil {
		t.Fatalf("FindPendingDedup mismatch: %v", err)
	}
	if ok {
		t.Fatal("expected no match for different size")
	}
}

func TestUploadSessionRepo_ListExpiredPending(t *testing.T) {
	db := setupSessionTestDB(t)
	ctx := context.Background()

	// An expired PENDING session.
	expired := &models.StorageUploadSession{
		OwnerType: 1, OwnerID: 500, Bucket: "b", ObjectKey: "o/exp",
		MD5: "exp", Size: 1, ContentType: "t", Filename: "f", Vendor: 1,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, expired); err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	// A still-valid PENDING session.
	active := &models.StorageUploadSession{
		OwnerType: 1, OwnerID: 500, Bucket: "b", ObjectKey: "o/act",
		MD5: "act", Size: 1, ContentType: "t", Filename: "f", Vendor: 1,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := CreateUploadSession(ctx, db, active); err != nil {
		t.Fatalf("Create active: %v", err)
	}

	got, err := ListExpiredPendingUploadSessions(ctx, db, time.Now(), 100)
	if err != nil {
		t.Fatalf("ListExpiredPending: %v", err)
	}
	if len(got) != 1 || got[0].ID != expired.ID {
		t.Fatalf("expected only the expired session, got %+v", got)
	}

	// GC marks it expired.
	claimed, err := MarkUploadSessionExpired(ctx, db, expired.ID)
	if err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if !claimed {
		t.Fatalf("MarkExpired: expected CAS to claim a PENDING session, got claimed=false")
	}
	got, err = ListExpiredPendingUploadSessions(ctx, db, time.Now(), 100)
	if err != nil {
		t.Fatalf("ListExpiredPending after expire: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no expired pending after MarkExpired, got %d", len(got))
	}

	// Re-running CAS on an already-EXPIRED session must report claimed=false
	// (the row is no longer PENDING) — this is the property GC relies on to
	// avoid racing confirmUpload.
	claimed2, err := MarkUploadSessionExpired(ctx, db, expired.ID)
	if err != nil {
		t.Fatalf("MarkExpired second call: %v", err)
	}
	if claimed2 {
		t.Fatalf("MarkExpired: second CAS on non-PENDING session must return claimed=false")
	}
}
