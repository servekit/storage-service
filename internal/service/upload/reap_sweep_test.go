package upload

import (
	"context"
	"testing"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReapExpiredSessions_SettledStagingCleanup covers the second GC phase:
// expired CONFIRMED/CANCELLED sessions must have their staging object
// reclaimed and the row retired, while the final-key object (the confirmed
// bytes) survives. This bounds the leak from a failed inline staging delete
// in ConfirmUpload and collects cancelled sessions' leftovers.
func TestReapExpiredSessions_SettledStagingCleanup(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	stagingConfirmed := "uploads/tmp/1-100/" + "11111111111111111111111111111111/" + victimMD5
	stagingCancelled := "uploads/tmp/1-200/" + "22222222222222222222222222222222/" + victimMD5

	confirmedSess := &models.StorageUploadSession{
		ID: 9101, OwnerType: 1, OwnerID: 100, Bucket: "uploads", ObjectKey: stagingConfirmed,
		MD5: victimMD5, Size: 4, Filename: "a.bin", ContentType: "text/plain", Vendor: 3,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED),
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	cancelledSess := &models.StorageUploadSession{
		ID: 9102, OwnerType: 1, OwnerID: 200, Bucket: "uploads", ObjectKey: stagingCancelled,
		MD5: victimMD5, Size: 4, Filename: "b.bin", ContentType: "text/plain", Vendor: 3,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CANCELLED),
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, dal.CreateUploadSession(ctx, db, confirmedSess))
	require.NoError(t, dal.CreateUploadSession(ctx, db, cancelledSess))

	// Both staging objects exist (the confirmed one simulates a failed inline
	// reclaim); the final key holds the live confirmed bytes.
	fp.PutObjectWithMD5(ctx, "uploads", stagingConfirmed, []byte("data"), "text/plain", victimMD5)
	fp.PutObjectWithMD5(ctx, "uploads", stagingCancelled, []byte("data"), "text/plain", victimMD5)
	fp.PutObjectWithMD5(ctx, "uploads", finalKey, []byte("data"), "text/plain", victimMD5)

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, deleted, "both staging objects must be reclaimed")

	assert.False(t, fp.ObjectExists("uploads", stagingConfirmed), "confirmed session staging object reclaimed")
	assert.False(t, fp.ObjectExists("uploads", stagingCancelled), "cancelled session staging object reclaimed")
	assert.True(t, fp.ObjectExists("uploads", finalKey), "final-key bytes must survive the sweep")

	// Rows are retired (soft-deleted) so future cycles stop rescanning them.
	_, err = dal.GetUploadSessionByID(ctx, db, confirmedSess.ID)
	assert.Error(t, err, "confirmed session row must be retired after cleanup")
	_, err = dal.GetUploadSessionByID(ctx, db, cancelledSess.ID)
	assert.Error(t, err, "cancelled session row must be retired after cleanup")

	// A second sweep is a no-op (rows retired).
	deleted2, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted2, "second sweep must find nothing")
}

// TestReapExpiredSessions_SettledLegacyRowsUntouched verifies settled rows
// whose ObjectKey is NOT a sandbox staging key (legacy pre-two-phase rows,
// content-addressed and globally deduped) are left entirely alone — neither
// the object nor the row.
func TestReapExpiredSessions_SettledLegacyRowsUntouched(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	legacyKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	sess := &models.StorageUploadSession{
		ID: 9201, OwnerType: 1, OwnerID: 100, Bucket: "uploads", ObjectKey: legacyKey,
		MD5: victimMD5, Size: 4, Filename: "legacy.bin", ContentType: "text/plain", Vendor: 3,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED),
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, dal.CreateUploadSession(ctx, db, sess))
	fp.PutObjectWithMD5(ctx, "uploads", legacyKey, []byte("data"), "text/plain", victimMD5)

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted, "content-addressed keys must never be deleted by the settled sweep")

	assert.True(t, fp.ObjectExists("uploads", legacyKey), "legacy object must survive")
	row, err := dal.GetUploadSessionByID(ctx, db, sess.ID)
	require.NoError(t, err, "legacy row must stay queryable")
	assert.Equal(t, int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED), row.Status)
}

// TestReapExpiredSessions_OtherOwnersSandboxNotReclaimed pins the owner
// scoping of the sweep: a staging key under owner A's sandbox attached to a
// settled session of owner B must not match B's sandbox prefix and is left
// for its owning session's own lifecycle (defensive — the key layout makes
// cross-owner rows impossible in practice).
func TestReapExpiredSessions_OtherOwnersSandboxNotReclaimed(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	ctx := context.Background()

	// Owner 200's settled session pointing at a key under owner 100's sandbox.
	foreignStaging := "uploads/tmp/1-100/" + "33333333333333333333333333333333/" + victimMD5
	sess := &models.StorageUploadSession{
		ID: 9301, OwnerType: 1, OwnerID: 200, Bucket: "uploads", ObjectKey: foreignStaging,
		MD5: victimMD5, Size: 4, Filename: "odd.bin", ContentType: "text/plain", Vendor: 3,
		Status:    int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CANCELLED),
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, dal.CreateUploadSession(ctx, db, sess))
	fp.PutObjectWithMD5(ctx, "uploads", foreignStaging, []byte("data"), "text/plain", victimMD5)

	deleted, err := svc.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, deleted, "foreign-sandbox keys must not be reclaimed by this session's sweep")
	assert.True(t, fp.ObjectExists("uploads", foreignStaging), "foreign staging object must survive")
}
