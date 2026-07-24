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

func setupObjectTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

func seedObjects(t *testing.T, db *gorm.DB, objs []models.StorageObject) []int64 {
	t.Helper()
	ids := make([]int64, len(objs))
	for i := range objs {
		if err := db.Create(&objs[i]).Error; err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
		ids[i] = objs[i].ID
	}
	return ids
}

func TestCountActiveAndSumObjectSizeByIDs_EmptyShortCircuit(t *testing.T) {
	db := setupObjectTestDB(t)
	row, err := CountActiveAndSumObjectSizeByIDs(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("empty ids: %v", err)
	}
	if row.TotalObjects != 0 || row.PhysicalBytes != 0 {
		t.Fatalf("empty ids: want zero row, got %+v", row)
	}
}

func TestCountActiveAndSumObjectSizeByIDs_CountsAndSums(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()

	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "image/png", StorageClass: 1, RefCount: 1},
		{Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "image/png", StorageClass: 1, RefCount: 1},
		{Vendor: 2, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "image/png", StorageClass: 1, RefCount: 1},
	})

	row, err := CountActiveAndSumObjectSizeByIDs(ctx, db, ids)
	if err != nil {
		t.Fatalf("CountActiveAndSumObjectSizeByIDs: %v", err)
	}
	if row.TotalObjects != 3 {
		t.Errorf("TotalObjects: got %d want 3", row.TotalObjects)
	}
	if row.PhysicalBytes != 700 {
		t.Errorf("PhysicalBytes: got %d want 700", row.PhysicalBytes)
	}
}

func TestCountActiveAndSumObjectSizeByIDs_SoftDeleteExcluded(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()

	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "t", StorageClass: 1, RefCount: 1},
		{Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "t", StorageClass: 1, RefCount: 1},
	})

	if err := DeleteObject(ctx, db, ids[0]); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	row, err := CountActiveAndSumObjectSizeByIDs(ctx, db, ids)
	if err != nil {
		t.Fatalf("after delete: %v", err)
	}
	if row.TotalObjects != 1 || row.PhysicalBytes != 200 {
		t.Errorf("after delete: want {1, 200}, got %+v", row)
	}
}

func TestGroupObjectsByVendorAndSumSize(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()

	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "t", StorageClass: 1, RefCount: 1},
		{Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "t", StorageClass: 1, RefCount: 1},
		{Vendor: 2, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "t", StorageClass: 1, RefCount: 1},
	})

	rows, err := GroupObjectsByVendorAndSumSize(ctx, db, ids)
	if err != nil {
		t.Fatalf("GroupObjectsByVendorAndSumSize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 vendor groups, got %d", len(rows))
	}
	byVendor := map[int32]models.ProviderStatRow{}
	for _, r := range rows {
		byVendor[r.Vendor] = r
	}
	if byVendor[1].ObjectCount != 2 || byVendor[1].TotalBytes != 300 {
		t.Errorf("vendor 1: got %+v", byVendor[1])
	}
	if byVendor[2].ObjectCount != 1 || byVendor[2].TotalBytes != 400 {
		t.Errorf("vendor 2: got %+v", byVendor[2])
	}
}

func TestGroupObjectsByBucketAndSumSize(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()

	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "t", StorageClass: 1, RefCount: 1},
		{Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "t", StorageClass: 1, RefCount: 1},
		{Vendor: 1, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "t", StorageClass: 1, RefCount: 1},
	})

	rows, err := GroupObjectsByBucketAndSumSize(ctx, db, ids)
	if err != nil {
		t.Fatalf("GroupObjectsByBucketAndSumSize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 bucket groups, got %d", len(rows))
	}
	byBucket := map[string]models.BucketObjectStatRow{}
	for _, r := range rows {
		byBucket[r.Bucket] = r
	}
	if byBucket["b1"].ObjectCount != 2 || byBucket["b1"].TotalBytes != 300 {
		t.Errorf("bucket b1: got %+v", byBucket["b1"])
	}
	if byBucket["b2"].ObjectCount != 1 || byBucket["b2"].TotalBytes != 400 {
		t.Errorf("bucket b2: got %+v", byBucket["b2"])
	}
}

// --- happy-path coverage for the remaining object.go functions ---

// TestGetObjectByID verifies active-row fetch + ErrObjectNotFound for missing.
func TestGetObjectByID(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k", MD5: "m", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	got, err := GetObjectByID(ctx, db, ids[0])
	if err != nil {
		t.Fatalf("GetObjectByID: %v", err)
	}
	if got.ID != ids[0] {
		t.Fatalf("got ID=%d, want %d", got.ID, ids[0])
	}

	if _, err := GetObjectByID(ctx, db, 999999); err == nil ||
		!errors.Is(err, xcodes.ErrObjectNotFound.New()) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

// TestFindObjectByVendorBucketMD5 verifies (found, true) and (nil, false) shapes.
func TestFindObjectByVendorBucketMD5(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k1", MD5: "md5-1", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	got, found, err := FindObjectByVendorBucketMD5(ctx, db, 1, "b", "md5-1")
	if err != nil || !found {
		t.Fatalf("want (found,true,nil), got (%v,%v,%v)", got, found, err)
	}
	if got.MD5 != "md5-1" {
		t.Fatalf("MD5 mismatch: %q", got.MD5)
	}

	if _, found, err := FindObjectByVendorBucketMD5(ctx, db, 1, "b", "missing"); err != nil || found {
		t.Fatalf("want (nil,false,nil) for missing, got (%v,%v,%v)", nil, found, err)
	}
}

// TestBatchGetObjectsByIDs verifies map result + empty-input short-circuit.
func TestBatchGetObjectsByIDs(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k1", MD5: "m1", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
		{Vendor: 1, Bucket: "b", ObjectKey: "k2", MD5: "m2", Size: 2, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	// Empty ids short-circuits to empty map.
	m, err := BatchGetObjectsByIDs(ctx, db, nil)
	if err != nil || len(m) != 0 {
		t.Fatalf("empty ids: want ({},nil), got (%+v,%v)", m, err)
	}

	m, err = BatchGetObjectsByIDs(ctx, db, ids)
	if err != nil {
		t.Fatalf("BatchGetObjectsByIDs: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("want 2 entries, got %d", len(m))
	}
	if _, ok := m[ids[0]]; !ok {
		t.Errorf("missing id %d in map", ids[0])
	}
}

// TestCreateOrGetObject verifies first call inserts and second call dedups.
func TestCreateOrGetObject(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()

	obj := &models.StorageObject{
		Vendor: 1, Bucket: "b", ObjectKey: "k", MD5: "md5",
		Size: 42, ContentType: "t", StorageClass: 1, RefCount: 0,
	}
	got, inserted, err := CreateOrGetObject(ctx, db, obj)
	if err != nil || !inserted {
		t.Fatalf("first call: want (obj,true,nil), got (%v,%v,%v)", got, inserted, err)
	}
	firstID := got.ID

	// Second call with same (vendor,bucket,md5) must dedup — same row, inserted=false.
	dup := &models.StorageObject{
		Vendor: 1, Bucket: "b", ObjectKey: "other-key", MD5: "md5",
		Size: 99, ContentType: "t", StorageClass: 1, RefCount: 0,
	}
	got2, inserted2, err := CreateOrGetObject(ctx, db, dup)
	if err != nil || inserted2 {
		t.Fatalf("dup call: want (obj,false,nil), got (%v,%v,%v)", got2, inserted2, err)
	}
	if got2.ID != firstID {
		t.Fatalf("dedup returned different ID: got %d, want %d", got2.ID, firstID)
	}
}

// TestIncrAndDecrObjectRefCount verifies atomic counter updates + guard against
// decrement below zero.
func TestIncrAndDecrObjectRefCount(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k", MD5: "m", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 1},
	})

	if err := IncrObjectRefCount(ctx, db, ids[0]); err != nil {
		t.Fatalf("Incr: %v", err)
	}
	got, _ := GetObjectByID(ctx, db, ids[0])
	if got.RefCount != 2 {
		t.Fatalf("after incr: want 2, got %d", got.RefCount)
	}

	if err := DecrObjectRefCount(ctx, db, ids[0]); err != nil {
		t.Fatalf("Decr: %v", err)
	}
	got, _ = GetObjectByID(ctx, db, ids[0])
	if got.RefCount != 1 {
		t.Fatalf("after decr: want 1, got %d", got.RefCount)
	}

	// Decr by 5 with only 1 remaining → guard fails.
	if err := DecrObjectRefCountBy(ctx, db, ids[0], 5); err == nil ||
		!errors.Is(err, xcodes.ErrObjectInsufficientRefCount.New()) {
		t.Fatalf("expected ErrObjectInsufficientRefCount, got %v", err)
	}
}

// TestFindObjectByObjectKey verifies bucket+key lookup returns ErrObjectNotFound
// when absent.
func TestFindObjectByObjectKey(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "key-1", MD5: "m", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	got, err := FindObjectByObjectKey(ctx, db, "b", "key-1")
	if err != nil {
		t.Fatalf("FindObjectByObjectKey: %v", err)
	}
	if got.ObjectKey != "key-1" {
		t.Fatalf("ObjectKey: %q", got.ObjectKey)
	}

	if _, err := FindObjectByObjectKey(ctx, db, "b", "missing"); err == nil ||
		!errors.Is(err, xcodes.ErrObjectNotFound.New()) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

// TestBatchFindObjectsByObjectKeys verifies map keyed by object_key + empty input.
func TestBatchFindObjectsByObjectKeys(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k1", MD5: "m1", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
		{Vendor: 1, Bucket: "b", ObjectKey: "k2", MD5: "m2", Size: 2, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	m, err := BatchFindObjectsByObjectKeys(ctx, db, "b", []string{"k1", "k2", "missing"})
	if err != nil {
		t.Fatalf("BatchFindObjectsByObjectKeys: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("want 2 entries, got %d", len(m))
	}
	if _, ok := m["k1"]; !ok {
		t.Error("missing k1")
	}

	// Empty keys → empty map, no error.
	m, err = BatchFindObjectsByObjectKeys(ctx, db, "b", nil)
	if err != nil || len(m) != 0 {
		t.Fatalf("empty keys: want ({},nil), got (%+v,%v)", m, err)
	}
}

// TestDeleteZeroRefCountObjects verifies bulk soft-delete targets only zero
// ref rows.
func TestDeleteZeroRefCountObjects(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k1", MD5: "m1", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
		{Vendor: 1, Bucket: "b", ObjectKey: "k2", MD5: "m2", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
		{Vendor: 1, Bucket: "b", ObjectKey: "k3", MD5: "m3", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 5},
	})

	n, err := DeleteZeroRefCountObjects(ctx, db)
	if err != nil {
		t.Fatalf("DeleteZeroRefCountObjects: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 deleted, got %d", n)
	}
}

// TestPurgeObject verifies hard delete of a soft-deleted row, plus rejection
// when the row isn't soft-deleted.
func TestPurgeObject(t *testing.T) {
	db := setupObjectTestDB(t)
	ctx := context.Background()
	ids := seedObjects(t, db, []models.StorageObject{
		{Vendor: 1, Bucket: "b", ObjectKey: "k", MD5: "m", Size: 1, ContentType: "t", StorageClass: 1, RefCount: 0},
	})

	// Not soft-deleted yet → ErrObjectNotSoftDeleted.
	if err := PurgeObject(ctx, db, ids[0]); err == nil ||
		!errors.Is(err, xcodes.ErrObjectNotSoftDeleted.New()) {
		t.Fatalf("expected ErrObjectNotSoftDeleted before soft delete, got %v", err)
	}

	if err := DeleteObject(ctx, db, ids[0]); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if err := PurgeObject(ctx, db, ids[0]); err != nil {
		t.Fatalf("PurgeObject after soft delete: %v", err)
	}

	// Verify the row is gone even via Unscoped.
	var count int64
	db.Unscoped().Model(&models.StorageObject{}).Where("id = ?", ids[0]).Count(&count)
	if count != 0 {
		t.Fatalf("row should be hard-deleted, still found %d", count)
	}
}
