package dal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupFileTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

// seedFile inserts one StorageFile via direct db.Create and returns it.
func seedFile(t *testing.T, db *gorm.DB, ownerType int32, ownerID int64, objectID int64, filename string) models.StorageFile {
	t.Helper()
	f := models.StorageFile{
		OwnerType: ownerType,
		OwnerID:   ownerID,
		ObjectID:  objectID,
		Filename:  filename,
	}
	if err := db.Create(&f).Error; err != nil {
		t.Fatalf("seed file %q: %v", filename, err)
	}
	return f
}

func TestFindFileOwnerObjectIDPairs_OrderAndContent(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	files := []models.StorageFile{
		{OwnerType: 1, OwnerID: 100, ObjectID: 1001, Filename: "f1"},
		{OwnerType: 1, OwnerID: 100, ObjectID: 1002, Filename: "f2"},
		{OwnerType: 1, OwnerID: 100, ObjectID: 1003, Filename: "f3"},
		{OwnerType: 1, OwnerID: 200, ObjectID: 2001, Filename: "f4"},
	}
	for i := range files {
		if err := db.Create(&files[i]).Error; err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}

	pairs, err := FindFileOwnerObjectIDPairs(ctx, db)
	if err != nil {
		t.Fatalf("FindFileOwnerObjectIDPairs: %v", err)
	}
	if len(pairs) != 4 {
		t.Fatalf("want 4 pairs, got %d", len(pairs))
	}

	// Order by id ascending — first pair matches first seeded file.
	if pairs[0].OwnerType != 1 || pairs[0].ObjectID != 1001 {
		t.Errorf("pairs[0]: got %+v", pairs[0])
	}
}

func TestFindFileOwnerObjectIDPairs_SoftDeleteExcluded(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	f1 := models.StorageFile{OwnerType: 1, OwnerID: 100, ObjectID: 1001, Filename: "f1"}
	f2 := models.StorageFile{OwnerType: 1, OwnerID: 100, ObjectID: 1002, Filename: "f2"}
	if err := db.Create(&f1).Error; err != nil {
		t.Fatalf("seed f1: %v", err)
	}
	if err := db.Create(&f2).Error; err != nil {
		t.Fatalf("seed f2: %v", err)
	}

	if err := DeleteFile(ctx, db, f1.ID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	pairs, err := FindFileOwnerObjectIDPairs(ctx, db)
	if err != nil {
		t.Fatalf("FindFileOwnerObjectIDPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("want 1 pair (soft-deleted excluded), got %d", len(pairs))
	}
	if pairs[0].ObjectID != 1002 {
		t.Errorf("pairs[0]: got %+v", pairs[0])
	}
}

// --- happy-path coverage for the remaining file.go functions ---

// TestCreateFile inserts a file via dal.CreateFile and verifies it round-trips.
func TestCreateFile(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	f := &models.StorageFile{
		OwnerType: 1, OwnerID: 100, ObjectID: 1001,
		Filename: "hello.txt", FilePath: "dir/hello.txt",
	}
	if err := CreateFile(ctx, db, f); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if f.ID == 0 {
		t.Fatal("expected ID set after CreateFile")
	}

	got, err := GetFileByID(ctx, db, f.ID)
	if err != nil {
		t.Fatalf("GetFileByID: %v", err)
	}
	if got.Filename != "hello.txt" || got.OwnerID != 100 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestGetFileByIDAndOwner verifies owner scoping: matching owner returns the
// row, mismatched owner returns ErrFileNotFound.
func TestGetFileByIDAndOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	f := seedFile(t, db, 1, 100, 1001, "f.txt")

	got, err := GetFileByIDAndOwner(ctx, db, f.ID, 100, 1)
	if err != nil {
		t.Fatalf("GetFileByIDAndOwner (match): %v", err)
	}
	if got.ID != f.ID {
		t.Fatalf("got ID=%d, want %d", got.ID, f.ID)
	}

	if _, err := GetFileByIDAndOwner(ctx, db, f.ID, 999, 1); err == nil {
		t.Fatal("expected error for mismatched owner, got nil")
	}
}

// TestGetFileByID_NotFound verifies ErrFileNotFound for a missing ID.
func TestGetFileByID_NotFound(t *testing.T) {
	db := setupFileTestDB(t)
	_, err := GetFileByID(context.Background(), db, 999999)
	if err == nil || !errors.Is(err, xcodes.ErrFileNotFound.New()) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

// TestListFilesByOwner verifies owner scoping + pagination normalization.
func TestListFilesByOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 100, 2, "b.txt")
	seedFile(t, db, 1, 200, 3, "c.txt") // different owner

	files, err := ListFilesByOwner(ctx, db, 100, 1, ListFilesFilter{}, nil)
	if err != nil {
		t.Fatalf("ListFilesByOwner: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want len=2, got len=%d", len(files))
	}

	// Empty objectIDs (not nil) short-circuits to empty result.
	files, err = ListFilesByOwner(ctx, db, 100, 1, ListFilesFilter{}, []int64{})
	if err != nil {
		t.Fatalf("ListFilesByOwner (empty objectIDs): %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("want short-circuit empty, got len=%d", len(files))
	}
}

// TestListFilesByOwner_FilenameOrderCursorCoversAllRows verifies the
// filename-order cursor walks every row across pages without drops or
// duplicates — the regression that fired when the cursor only encoded
// `id < afterID` while ORDER BY used filename.
//
// Setup: 5 files named z.., y.., x.., w.., v.. inserted with non-monotonic
// IDs (filename order ≠ id order). Page size = 2. We page through with the
// filename cursor and assert the union matches the seed set.
func TestListFilesByOwner_FilenameOrderCursorCoversAllRows(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	// Seed 5 files with filenames sorted descending: zebra > yankee > xray > whiskey > victor.
	// Snowflake-ish IDs come from seedFile's auto-increment and go 1→5 in
	// insertion order, but insertion order is NOT filename order — exactly
	// the case where ID-cursor under filename-ORDER BY drops rows.
	seedFile(t, db, 1, 100, 10, "victor")  // id=1, filename=victor
	seedFile(t, db, 1, 100, 20, "whiskey") // id=2
	seedFile(t, db, 1, 100, 30, "xray")    // id=3
	seedFile(t, db, 1, 100, 40, "yankee")  // id=4
	seedFile(t, db, 1, 100, 50, "zebra")   // id=5

	wantNames := map[string]bool{
		"victor": true, "whiskey": true, "xray": true, "yankee": true, "zebra": true,
	}

	pageSize := 2
	gotNames := make(map[string]bool)
	pages := 0

	filter := ListFilesFilter{
		OrderBy:    storagev1.SortField_SORT_FIELD_FILENAME,
		Descending: true,
		Pagination: dbx.Pagination{PageSize: pageSize},
	}
	files, err := ListFilesByOwner(ctx, db, 100, 1, filter, nil)
	if err != nil {
		t.Fatalf("page %d: %v", pages, err)
	}
	if len(files) == 0 {
		t.Fatalf("page %d: empty first page", pages)
	}

	// Subsequent pages: cursor from previous page's last row. DAL fetches
	// pageSize+1 rows so callers can detect "has-next"; we collect the
	// pageSize trimmed rows, then advance the cursor while has-next is true.
	for {
		// Collect this page's rows (trim has-next hint if present).
		trim := files
		hasNext := len(files) > pageSize
		if hasNext {
			trim = files[:pageSize]
		}
		for _, f := range trim {
			t.Logf("  page %d row: id=%d filename=%q", pages, f.ID, f.Filename)
			if gotNames[f.Filename] {
				t.Fatalf("page %d: duplicate filename %q across pages", pages, f.Filename)
			}
			gotNames[f.Filename] = true
		}
		if !hasNext {
			break
		}
		last := files[pageSize-1]
		t.Logf("cursor: afterFilename=%q afterID=%d", last.Filename, last.ID)
		filter.AfterID = last.ID
		filter.AfterFilename = last.Filename
		files, err = ListFilesByOwner(ctx, db, 100, 1, filter, nil)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > 10 {
			t.Fatalf("page %d: cursor loop did not converge", pages)
		}
	}

	if !mapsEqual(gotNames, wantNames) {
		t.Fatalf("cursor missed rows: got=%v want=%v", gotNames, wantNames)
	}
}

// TestListFilesByOwnerPaged verifies offset pagination: each page returns the
// correct slice, total_count is accurate, and ordering is stable across pages.
func TestListFilesByOwnerPaged(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	// Seed 25 files for owner (type=1, id=100), names chosen so filename-asc
	// ordering is deterministic: f00, f01, ..., f24.
	for i := 0; i < 25; i++ {
		seedFile(t, db, 1, 100, int64(1000+i), fmt.Sprintf("f%02d", i))
	}
	// Different owner — must not appear in results.
	seedFile(t, db, 1, 200, 9999, "other")

	// Page 1: first 10 files in filename-asc order.
	files, total, err := ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		OrderBy:    storagev1.SortField_SORT_FIELD_FILENAME,
		PageParams: dbx.PageParams{Page: 1, PageSize: 10, Count: true},
	}, nil)
	require.NoError(t, err)
	require.Len(t, files, 10)
	assert.Equal(t, int64(25), total)
	assert.Equal(t, "f00", files[0].Filename)
	assert.Equal(t, "f09", files[9].Filename)

	// Page 3: last 5 files.
	files, _, err = ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		OrderBy:    storagev1.SortField_SORT_FIELD_FILENAME,
		PageParams: dbx.PageParams{Page: 3, PageSize: 10, Count: true},
	}, nil)
	require.NoError(t, err)
	require.Len(t, files, 5)
	assert.Equal(t, "f20", files[0].Filename)
	assert.Equal(t, "f24", files[4].Filename)

	// Page 4: out of range — empty list, but no error.
	files, _, err = ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		OrderBy:    storagev1.SortField_SORT_FIELD_FILENAME,
		PageParams: dbx.PageParams{Page: 4, PageSize: 10, Count: true},
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestListFilesByOwnerPaged_PathPrefix verifies the path_prefix filter is
// applied before COUNT and LIMIT.
//
// seedFile does not populate FilePath, so we seed manually here via db.Create
// to set both Filename and FilePath.
func TestListFilesByOwnerPaged_PathPrefix(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	seedFileWithPath(t, db, 1, 100, 1, "a.txt", "docs/a.txt")
	seedFileWithPath(t, db, 1, 100, 2, "b.txt", "docs/b.txt")
	seedFileWithPath(t, db, 1, 100, 3, "c.jpg", "photos/c.jpg")

	files, total, err := ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		PathPrefix: "docs/",
		PageParams: dbx.PageParams{Page: 1, PageSize: 10, Count: true},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	require.Len(t, files, 2)
}

// TestListFilesByOwnerPaged_EmptyObjectIDs verifies the early-exit short
// circuit: when ContentTypePrefix resolves to no objects, the DAL returns
// (nil, 0, nil) without hitting the DB.
func TestListFilesByOwnerPaged_EmptyObjectIDs(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	seedFile(t, db, 1, 100, 1, "a.txt")

	files, total, err := ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		PageParams: dbx.PageParams{Page: 1, PageSize: 10, Count: true},
	}, []int64{})
	require.NoError(t, err)
	assert.Empty(t, files)
	assert.Equal(t, int64(0), total)
}

// TestListFilesByOwnerPaged_SkipCount verifies that Count=false skips the
// COUNT(*) query — total comes back as 0 without error.
func TestListFilesByOwnerPaged_SkipCount(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 100, 2, "b.txt")

	files, total, err := ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		PageParams: dbx.PageParams{Page: 1, PageSize: 10, Count: false},
	}, nil)
	require.NoError(t, err)
	require.Len(t, files, 2)
	assert.Equal(t, int64(0), total, "total must be 0 when Count is false")
}

// TestListFilesByOwnerPaged_SizeOrderFallback confirms that selecting
// SORT_FIELD_SIZE falls through to (created_at, id) order, matching the
// applyFileOrder default branch. Without a join to StorageObject we can't
// sort by actual size, so the contract is "deterministic, no error".
func TestListFilesByOwnerPaged_SizeOrderFallback(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()

	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 100, 2, "b.txt")
	seedFile(t, db, 1, 100, 3, "c.txt")

	files, total, err := ListFilesByOwnerPaged(ctx, db, 100, 1, ListFilesPagedFilter{
		OrderBy:    storagev1.SortField_SORT_FIELD_SIZE,
		PageParams: dbx.PageParams{Page: 1, PageSize: 10, Count: true},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, files, 3)
	// Expect created_at-asc order (fallback). IDs are assigned in insertion
	// order, so files come back in seed order.
	assert.Equal(t, "a.txt", files[0].Filename)
	assert.Equal(t, "b.txt", files[1].Filename)
	assert.Equal(t, "c.txt", files[2].Filename)
}

// seedFileWithPath inserts one StorageFile with both Filename and FilePath set.
// Used by tests that exercise the PathPrefix filter (seedFile only sets Filename).
func seedFileWithPath(t *testing.T, db *gorm.DB, ownerType int32, ownerID int64, objectID int64, filename, filePath string) models.StorageFile {
	t.Helper()
	f := models.StorageFile{
		OwnerType: ownerType,
		OwnerID:   ownerID,
		ObjectID:  objectID,
		Filename:  filename,
		FilePath:  filePath,
	}
	if err := db.Create(&f).Error; err != nil {
		t.Fatalf("seed file %q: %v", filename, err)
	}
	return f
}

// TestListAllFiles verifies admin listing returns every file regardless of owner.
func TestListAllFiles(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 200, 2, "b.txt")
	seedFile(t, db, 2, 300, 3, "c.txt")

	files, total, err := ListAllFiles(ctx, db, AdminListFilesFilter{}, nil)
	if err != nil {
		t.Fatalf("ListAllFiles: %v", err)
	}
	if total != 3 || len(files) != 3 {
		t.Fatalf("want total=3 len=3, got total=%d len=%d", total, len(files))
	}

	// Filter narrows.
	files, total, err = ListAllFiles(ctx, db, AdminListFilesFilter{OwnerType: 1, OwnerID: 100}, nil)
	if err != nil {
		t.Fatalf("ListAllFiles (filtered): %v", err)
	}
	if total != 1 || len(files) != 1 {
		t.Fatalf("want total=1 len=1 after filter, got total=%d len=%d", total, len(files))
	}
}

// TestUpdateFile verifies an existing row is updated and rowsAffected check
// catches missing rows.
func TestUpdateFile(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	f := seedFile(t, db, 1, 100, 1, "old.txt")

	f.Filename = "new.txt"
	if err := UpdateFile(ctx, db, &f); err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}

	got, err := GetFileByID(ctx, db, f.ID)
	if err != nil {
		t.Fatalf("GetFileByID: %v", err)
	}
	if got.Filename != "new.txt" {
		t.Fatalf("Filename not updated: got %q", got.Filename)
	}

	// Mismatched owner (OwnerID in WHERE) → ErrFileNotActive.
	other := f
	other.OwnerID = 999
	if err := UpdateFile(ctx, db, &other); err == nil || !errors.Is(err, xcodes.ErrFileNotActive.New()) {
		t.Fatalf("expected ErrFileNotActive for stale row, got %v", err)
	}
}

// TestDeleteFile verifies soft delete; subsequent Get returns ErrFileNotFound.
func TestDeleteFile(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	f := seedFile(t, db, 1, 100, 1, "x.txt")

	if err := DeleteFile(ctx, db, f.ID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := GetFileByID(ctx, db, f.ID); err == nil || !errors.Is(err, xcodes.ErrFileNotFound.New()) {
		t.Fatalf("expected ErrFileNotFound after delete, got %v", err)
	}

	// Deleting a non-existent id returns ErrFileNotActive.
	if err := DeleteFile(ctx, db, 999999); err == nil || !errors.Is(err, xcodes.ErrFileNotActive.New()) {
		t.Fatalf("expected ErrFileNotActive for missing id, got %v", err)
	}
}

// TestBatchDeleteFiles verifies count + owner scoping on bulk delete.
func TestBatchDeleteFiles(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	f1 := seedFile(t, db, 1, 100, 1, "a.txt")
	f2 := seedFile(t, db, 1, 100, 2, "b.txt")
	seedFile(t, db, 1, 200, 3, "c.txt") // different owner — must NOT be touched

	n, err := BatchDeleteFiles(ctx, db, []int64{f1.ID, f2.ID, 9999}, 100, 1)
	if err != nil {
		t.Fatalf("BatchDeleteFiles: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows affected, got %d", n)
	}

	// Empty ids short-circuit.
	n, err = BatchDeleteFiles(ctx, db, []int64{}, 100, 1)
	if err != nil || n != 0 {
		t.Fatalf("empty ids: want (0,nil), got (%d,%v)", n, err)
	}
}

// TestCountFilesByOwner verifies owner scoping of the count.
func TestCountFilesByOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 100, 2, "b.txt")
	seedFile(t, db, 1, 200, 3, "c.txt")

	n, err := CountFilesByOwner(ctx, db, 100, 1)
	if err != nil {
		t.Fatalf("CountFilesByOwner: %v", err)
	}
	if n != 2 {
		t.Fatalf("want count=2, got %d", n)
	}
}

// TestGetFileObjectRefCountsByOwner verifies the objectID→count map aggregates
// multiple files referencing the same object.
func TestGetFileObjectRefCountsByOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 11, "a.txt")
	seedFile(t, db, 1, 100, 11, "b.txt") // same object 11 → count 2
	seedFile(t, db, 1, 100, 22, "c.txt")
	seedFile(t, db, 1, 200, 11, "d.txt") // different owner — excluded

	counts, err := GetFileObjectRefCountsByOwner(ctx, db, 1, 100)
	if err != nil {
		t.Fatalf("GetFileObjectRefCountsByOwner: %v", err)
	}
	if got := counts[11]; got != 2 {
		t.Errorf("object 11: want count 2, got %d", got)
	}
	if got := counts[22]; got != 1 {
		t.Errorf("object 22: want count 1, got %d", got)
	}
	if _, ok := counts[33]; ok {
		t.Error("did not expect object 33 in counts")
	}
}

// TestDeleteFilesByOwner verifies bulk delete by owner returns the affected count.
func TestDeleteFilesByOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 1, "a.txt")
	seedFile(t, db, 1, 100, 2, "b.txt")
	seedFile(t, db, 1, 200, 3, "c.txt")

	n, err := DeleteFilesByOwner(ctx, db, 1, 100)
	if err != nil {
		t.Fatalf("DeleteFilesByOwner: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 affected, got %d", n)
	}

	remaining, err := CountFilesByOwner(ctx, db, 100, 1)
	if err != nil {
		t.Fatalf("CountFilesByOwner after delete: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("owner should have 0 files, got %d", remaining)
	}
}

// TestFindFileObjectIDsByOwner verifies the slice contains all object_ids of
// the owner's active files (duplicates allowed).
func TestFindFileObjectIDsByOwner(t *testing.T) {
	db := setupFileTestDB(t)
	ctx := context.Background()
	seedFile(t, db, 1, 100, 11, "a.txt")
	seedFile(t, db, 1, 100, 22, "b.txt")
	seedFile(t, db, 1, 100, 11, "c.txt") // dup object_id 11
	seedFile(t, db, 1, 200, 33, "d.txt") // different owner

	ids, err := FindFileObjectIDsByOwner(ctx, db, 1, 100)
	if err != nil {
		t.Fatalf("FindFileObjectIDsByOwner: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 ids, got %d (%v)", len(ids), ids)
	}
}

// silence unused import warning if SortField ends up unused
var _ = storagev1.SortField_SORT_FIELD_FILENAME

// mapsEqual is a small helper for set comparison in the cursor test.
func mapsEqual(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}
