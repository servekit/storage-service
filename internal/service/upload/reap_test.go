package upload

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReapExpiredSessions_OrphanCleanup verifies: a PENDING session past expiry whose OSS
// object exists → object is deleted, session marked EXPIRED.
func TestReapExpiredSessions_OrphanCleanup(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	// Create an expired session directly via repo.
	sess := &models.StorageUploadSession{
		ID:          1,
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "uploads",
		ObjectKey:   "uploads/abc",
		MD5:         "00000000000000000000000000000001",
		Size:        10,
		Filename:    "a.txt",
		ContentType: "text/plain",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(-time.Minute), // already expired
	}
	err := dal.CreateUploadSession(ctx, db, sess)
	require.NoError(t, err)

	// Simulate client having uploaded to OSS (orphan).
	fp.PutObjectWithMD5(context.Background(), "uploads", "uploads/abc", []byte("data"), "text/plain", "00000000000000000000000000000001")
	require.True(t, fp.ObjectExists("uploads", "uploads/abc"), "precondition: object must exist")

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "one orphan should be deleted")
	assert.False(t, fp.ObjectExists("uploads", "uploads/abc"), "object should be deleted from fake provider")

	// Verify session is now EXPIRED.
	s, err := dal.GetUploadSessionByID(ctx, db, 1)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED)), s.Status)
}

// TestReapExpiredSessions_NoUploadSkipsDelete verifies: a PENDING session past expiry whose
// OSS object does NOT exist (client never uploaded) → just mark EXPIRED, no delete attempt.
func TestReapExpiredSessions_NoUploadSkipsDelete(t *testing.T) {
	svc, _, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	sess := &models.StorageUploadSession{
		ID:          2,
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "uploads",
		ObjectKey:   "uploads/never-uploaded",
		MD5:         "00000000000000000000000000000002",
		Size:        5,
		Filename:    "b.txt",
		ContentType: "text/plain",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(-time.Minute),
	}
	err := dal.CreateUploadSession(ctx, db, sess)
	require.NoError(t, err)

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted, "no orphan to delete")

	s, err := dal.GetUploadSessionByID(ctx, db, 2)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED)), s.Status)
}

// TestReapExpiredSessions_ConfirmedSessionNotDeletedRaceFix verifies the critical race
// condition fix: when a session has been concurrently CONFIRMED (file/object
// rows now point at the OSS key), ReapExpiredSessions MUST NOT delete the OSS object.
// Pre-fix: GC ran HeadObject → succeeded → confirmUpload committed → GC
// DeleteObject → silent data loss (download 404).
// Post-fix: GC's MarkExpired CAS fails (status != PENDING) → cloud delete
// skipped → OSS data preserved.
func TestReapExpiredSessions_ConfirmedSessionNotDeletedRaceFix(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	// Expired-PENDING session whose OSS object is already in place.
	sess := &models.StorageUploadSession{
		ID:          4,
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "uploads",
		ObjectKey:   "uploads/race",
		MD5:         "00000000000000000000000000000004",
		Size:        4,
		Filename:    "d.txt",
		ContentType: "text/plain",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(-time.Minute),
	}
	err := dal.CreateUploadSession(ctx, db, sess)
	require.NoError(t, err)
	fp.PutObjectWithMD5(context.Background(), "uploads", "uploads/race",
		[]byte("data"), "text/plain", "00000000000000000000000000000004")
	require.True(t, fp.ObjectExists("uploads", "uploads/race"), "precondition: object must exist")

	// Simulate confirmUpload winning the race by transitioning to CONFIRMED
	// BEFORE ReapExpiredSessions reaches its CAS.
	require.NoError(t, dal.MarkUploadSessionConfirmed(ctx, db, sess.ID, 9999))

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted, "confirmed session must not have its OSS object deleted")

	// The OSS object MUST survive — confirmUpload's file/object rows depend on it.
	assert.True(t, fp.ObjectExists("uploads", "uploads/race"),
		"OSS object must survive GC when session is no longer PENDING")

	// And the session must still be CONFIRMED (not overwritten to EXPIRED).
	s, err := dal.GetUploadSessionByID(ctx, db, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED)), s.Status,
		"GC must not overwrite CONFIRMED status")
}

// TestReapExpiredSessions_TransientErrorRetries verifies that when HeadObject returns a
// transient error (NOT ErrObjectNotFound), the session is left PENDING so the
// next GC cycle retries — instead of being silently expired and leaking OSS
// storage on a real orphan.
func TestReapExpiredSessions_TransientErrorRetries(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	sess := &models.StorageUploadSession{
		ID:          3,
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "uploads",
		ObjectKey:   "uploads/flaky",
		MD5:         "00000000000000000000000000000003",
		Size:        5,
		Filename:    "c.txt",
		ContentType: "text/plain",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(-time.Minute),
	}
	err := dal.CreateUploadSession(ctx, db, sess)
	require.NoError(t, err)

	// Make HeadObject return a transient error (not ErrObjectNotFound) for
	// this key — simulating a network blip / OSS 5xx / timeout.
	fp.SetHeadObjectError("uploads", "uploads/flaky", errors.New("simulated OSS timeout"))

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted, "no orphan deleted on transient error")

	// Session must STILL be PENDING — next GC cycle retries once OSS recovers.
	s, err := dal.GetUploadSessionByID(ctx, db, 3)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING)), s.Status,
		"session must remain PENDING on transient HeadObject error so next GC cycle retries")

	// And a second GC run, now that OSS is reachable again with the object
	// genuinely absent, must complete and expire the session.
	fp.SetHeadObjectError("uploads", "uploads/flaky", nil)

	deleted2, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted2, "no orphan to delete after recovery")

	s2, err := dal.GetUploadSessionByID(ctx, db, 3)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED)), s2.Status,
		"session must expire once HeadObject confirms not-found on retry")
}

// TestReapExpiredSessions_HAReplicasDoNotDoubleProcess verifies the I7 fix: when two HA
// replicas trigger GC concurrently, the advisory lock ensures only one of
// them performs the OSS work. The other either fails to acquire the lease
// (returns 0 immediately) or finds the session already EXPIRED by the winner
// (MarkExpired CAS fails → no cloud delete). Either way, the OSS orphan is
// deleted exactly once across both replicas.
//
// Pre-fix: both replicas would scan the same batch, both call HeadObject +
// DeleteObject on the same key. OSS DeleteObject is idempotent so no data
// corruption, but the redundant OSS calls are real cost and noise.
func TestReapExpiredSessions_HAReplicasDoNotDoubleProcess(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	sess := &models.StorageUploadSession{
		ID:          5,
		OwnerType:   1,
		OwnerID:     100,
		Bucket:      "uploads",
		ObjectKey:   "uploads/ha-contention",
		MD5:         "00000000000000000000000000000005",
		Size:        6,
		Filename:    "e.txt",
		ContentType: "text/plain",
		Vendor:      1,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(-time.Minute),
	}
	err := dal.CreateUploadSession(ctx, db, sess)
	require.NoError(t, err)
	fp.PutObjectWithMD5(context.Background(), "uploads", "uploads/ha-contention",
		[]byte("data"), "text/plain", "00000000000000000000000000000005")
	require.True(t, fp.ObjectExists("uploads", "uploads/ha-contention"),
		"precondition: object must exist")

	var deleted1, deleted2 int
	var err1, err2 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		deleted1, err1 = svc.ReapExpiredSessions(ctx)
	}()
	go func() {
		defer wg.Done()
		deleted2, err2 = svc.ReapExpiredSessions(ctx)
	}()
	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)

	// Exactly one of the two replicas reports having deleted the orphan.
	// The other either lost the advisory lock (returned 0,nil immediately) or
	// lost the per-row CAS (MarkExpired returned claimed=false → skipped).
	assert.Equal(t, 1, deleted1+deleted2,
		"exactly one replica should delete the orphan; got d1=%d d2=%d", deleted1, deleted2)

	// And the OSS object is gone (deleted exactly once).
	assert.False(t, fp.ObjectExists("uploads", "uploads/ha-contention"),
		"orphan should be deleted after contention")

	// Session must be EXPIRED regardless of which replica won.
	s, err := dal.GetUploadSessionByID(ctx, db, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED)), s.Status,
		"session must end up EXPIRED")
}
