package upload

import (
	"context"
	"testing"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- uploadtoken tests ---
//
// Migrated from internal/service/service_test.go. The token sign/verify logic
// is self-contained (stdlib crypto only), so these unit tests need no Docker.

func TestSignAndVerifyToken(t *testing.T) {
	token := &uploadToken{
		OwnerID:     1001,
		OwnerType:   1,
		MD5:         "d41d8cd98f00b204e9800998ecf8427e",
		Size:        1024,
		ContentType: "image/png",
		Bucket:      "default",
		Vendor:      int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
		Filename:    "test.png",
		FilePath:    "/images/test.png",
		ExpiresAt:   time.Now().Add(30 * time.Minute).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)
	assert.NotEmpty(t, encoded)

	verified, err := verifyUploadToken(encoded, testSecret, 1001, 1)
	require.NoError(t, err)
	assert.Equal(t, token.OwnerID, verified.OwnerID)
	assert.Equal(t, token.OwnerType, verified.OwnerType)
	assert.Equal(t, token.MD5, verified.MD5)
	assert.Equal(t, token.Size, verified.Size)
	assert.Equal(t, token.ContentType, verified.ContentType)
	assert.Equal(t, token.Bucket, verified.Bucket)
	assert.Equal(t, token.Vendor, verified.Vendor)
	assert.Equal(t, token.Filename, verified.Filename)
}

func TestVerifyToken_WrongSecret(t *testing.T) {
	token := &uploadToken{
		OwnerID:   1001,
		MD5:       "abc123",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)

	_, err = verifyUploadToken(encoded, "wrong-secret", 1001, 1)
	assert.Error(t, err)
	assert.True(t, isUploadTokenInvalid(err))
}

func TestVerifyToken_Expired(t *testing.T) {
	token := &uploadToken{
		OwnerID:   1001,
		MD5:       "abc123",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)

	_, err = verifyUploadToken(encoded, testSecret, 1001, 1)
	assert.Error(t, err)
	assert.True(t, isUploadTokenExpired(err))
}

func TestVerifyToken_OwnerIDMismatch(t *testing.T) {
	token := &uploadToken{
		OwnerID:   1001,
		MD5:       "abc123",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)

	_, err = verifyUploadToken(encoded, testSecret, 9999, 1)
	assert.Error(t, err)
	assert.True(t, isUploadTokenInvalid(err))
}

// TestVerifyToken_OwnerTypeMismatch covers the cross-owner_type confusion
// regression: token carries OwnerType=1 (user) but caller claims OwnerType=2
// (tenant) with the same OwnerID. Without an OwnerType check, the caller can
// confirm/cancel a session that belongs to a different owner_type but
// happens to share the integer OwnerID.
func TestVerifyToken_OwnerTypeMismatch(t *testing.T) {
	token := &uploadToken{
		OwnerID:   1001,
		OwnerType: 1, // signed for owner_type=1
		MD5:       "abc123",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)

	// Same OwnerID, different OwnerType — must be rejected.
	_, err = verifyUploadToken(encoded, testSecret, 1001, 2)
	assert.Error(t, err)
	assert.True(t, isUploadTokenInvalid(err))
}

func TestVerifyToken_TamperedPayload(t *testing.T) {
	token := &uploadToken{
		OwnerID:   1001,
		MD5:       "abc123",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	encoded, err := signUploadToken(token, testSecret)
	require.NoError(t, err)

	tampered := encoded[:len(encoded)-5] + "XXXXX"
	_, err = verifyUploadToken(tampered, testSecret, 1001, 1)
	assert.Error(t, err)
	assert.True(t, isUploadTokenInvalid(err))
}

func TestVerifyToken_InvalidFormat(t *testing.T) {
	_, err := verifyUploadToken("no-dot-here", testSecret, 1001, 1)
	assert.Error(t, err)
	assert.True(t, isUploadTokenInvalid(err))
}

// TestCheckUploadRateLimit_noLimiter verifies the no-limiter fast path returns
// nil immediately. Migrated from the parent package; checkUploadRateLimit now
// lives on *upload.Service.
func TestCheckUploadRateLimit_noLimiter(t *testing.T) {
	svc := &Service{}
	assert.NoError(t, svc.checkUploadRateLimit(context.TODO(), 1, 123))
}
