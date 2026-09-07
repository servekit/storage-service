package upload

import (
	"context"
	"strings"
	"testing"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetSTSCredential_ExtensionRejectedEarly verifies the fail-fast path:
// filename's extension not in allowed_extensions → BAD_REQUEST before any
// STS call (saves AssumeRole quota).
func TestGetSTSCredential_ExtensionRejectedEarly(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	_, err := svc.GetSTSCredential(context.Background(), &storagev1.GetSTSCredentialRequest{
		Owner:             &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:            "uploads",
		MaxSize:           1024,
		Md5:               "00000000000000000000000000000001",
		ContentType:       "text/plain",
		Filename:          "photo.exe",
		AllowedExtensions: []string{".jpg", ".png"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
	assert.Contains(t, err.Error(), ".exe")
	assert.Equal(t, 0, fp.STSCalls(), "fail-fast must not reach the provider")
}

// TestGetSTSCredential_ExtensionCaseInsensitiveMatch verifies the
// acceptance criterion: ".JPG" filename matches [".jpg"] allowlist after
// service-layer normalization. Confirms fail-fast does NOT fire.
func TestGetSTSCredential_ExtensionCaseInsensitiveMatch(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	_, err := svc.GetSTSCredential(context.Background(), &storagev1.GetSTSCredentialRequest{
		Owner:             &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:            "uploads",
		MaxSize:           1024,
		Md5:               "00000000000000000000000000000003",
		ContentType:       "image/jpeg",
		Filename:          "photo.JPG",
		AllowedExtensions: []string{".jpg"},
	})
	require.NoError(t, err, "uppercase filename extension must match lowercase allowlist entry")
}

// TestBatchGetSTSCredential_ExtensionRejectedPerItem verifies the batch
// fail-fast path: a file whose extension is not in allowed_extensions gets
// an ItemError (BAD_REQUEST) without aborting the rest of the batch, and
// without burning an STS issuer call for that file.
func TestBatchGetSTSCredential_ExtensionRejectedPerItem(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Files: []*storagev1.UploadFileMeta{
			{Filename: "ok.jpg", Md5: "00000000000000000000000000000001", Size: 1},
			{Filename: "bad.exe", Md5: "00000000000000000000000000000002", Size: 1},
		},
		AllowedExtensions: []string{".jpg"},
	})
	require.NoError(t, err) // per-item error, not batch-level
	require.Len(t, resp.GetItems(), 2)

	// First file: allowed extension, no error.
	assert.Nil(t, resp.GetItems()[0].GetError(), "allowed-extension file must not produce ItemError")

	// Second file: disallowed extension → ItemError with BAD_REQUEST code.
	itemErr := resp.GetItems()[1].GetError()
	require.NotNil(t, itemErr, "disallowed-extension file must produce ItemError")
	assert.Contains(t, itemErr.GetCode(), "BAD_REQUEST")
	assert.Contains(t, itemErr.GetMessage(), ".exe")

	// Only the allowed file should have reached the STS issuer; the rejected
	// one must fail-fast before provider call. (Batch also issues a shared STS
	// credential, so the call count is 1 from the shared path and at most 1
	// from the per-file path of the accepted file. The rejected file never
	// reaches the provider.)
	//
	// Note: FakeProvider.GetSTSToken is invoked via sts.Get, which is cached
	// per (owner, vendor, bucket). Both the shared batch STS call and the
	// per-file issueUploadCredential hit the same cache slot, so total calls
	// collapse to 1 (the cache-priming call). We assert <= 1 to allow either
	// execution order without overspecifying.
	assert.LessOrEqual(t, fp.STSCalls(), 1, "disallowed-extension file must not reach the provider")
}

// TestBatchGetSTSCredential_ObjectKeyPopulated verifies that batch token
// items carry the full OSS object key (not just the keyPrefix), so clients
// can PUT to the right path without knowing the md5-sharding rule.
func TestBatchGetSTSCredential_ObjectKeyPopulated(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	const md5 = "00000000000000000000000000000001"
	resp, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Files: []*storagev1.UploadFileMeta{
			{Filename: "photo.jpg", Md5: md5, Size: 1, ContentType: "image/jpeg"},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.GetItems(), 1)

	token := resp.GetItems()[0].GetToken()
	require.NotNil(t, token, "expected a token item, got error or instant file_id")
	// Two-phase: object_key is an unguessable staging key under the owner's
	// sandbox — "<keyPrefix>tmp/<ownerType>-<ownerID>/<random32>/<md5>" — NOT
	// the derivable content-addressed key. Assert the sandbox shape and that
	// the derivable final key ("uploads/00/<md5>") is never handed out.
	key := token.GetObjectKey()
	sandbox := "uploads/tmp/1-100/"
	assert.True(t, strings.HasPrefix(key, sandbox),
		"object_key %q must live under the owner staging sandbox %q", key, sandbox)
	assert.True(t, strings.HasSuffix(key, "/"+md5), "object_key %q must end with the declared md5", key)
	assert.Len(t, strings.TrimSuffix(strings.TrimPrefix(key, sandbox), "/"+md5), 32,
		"staging key must carry a 128-bit random segment")
	assert.NotEqual(t, "uploads/00/"+md5, key,
		"the content-addressed final key must never be exposed to clients")
}

// TestNormalizeExtensions verifies trim/lowercase/empty-filter behavior.
func TestNormalizeExtensions(t *testing.T) {
	got := normalizeExtensions([]string{" .JPG ", "PNG", "", ".pdf"})
	assert.Equal(t, []string{".jpg", "png", ".pdf"}, got)
}

// TestIsPublicACL covers the ACL classifier used by ConfirmUpload's privacy
// check. Empty and "default" (Aliyun's "inherit bucket default") must return
// false — ConfirmUpload treats these as "no public grant verified".
func TestIsPublicACL(t *testing.T) {
	cases := []struct {
		acl  string
		want bool
	}{
		{"", false},
		{"private", false},
		{"default", false},
		{"public-read", true},
		{"public-read-write", true},
		{"PUBLIC-READ", true}, // case-insensitive
	}
	for _, tc := range cases {
		t.Run(tc.acl, func(t *testing.T) {
			assert.Equal(t, tc.want, isPublicACL(tc.acl))
		})
	}
}

// TestGenerateUploadURL_VisibilityPublicUsesPublicBucket verifies the
// visibility=PUBLIC mapping: the presigned PUT must target the configured
// public bucket (caller bucket field ignored) and the session snapshot must
// mark the file public.
func TestGenerateUploadURL_VisibilityPublicUsesPublicBucket(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, err := svc.GenerateUploadURL(context.Background(), &storagev1.GenerateUploadURLRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 300},
		Filename:    "avatar.png",
		Md5:         "10000000000000000000000000000001",
		Size:        16,
		ContentType: "image/png",
		Bucket:      "uploads", // deliberately private; PUBLIC must override it
		Visibility:  storagev1.Visibility_VISIBILITY_PUBLIC,
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant(), "fresh md5 must not instant-upload")
	require.Contains(t, resp.GetUploadUrl(), "public-uploads", "PUBLIC upload must land in the public bucket")
	_ = fp

	// The persisted session must carry the public bucket so ConfirmUpload
	// verifies and persists is_public=true.
	var sess models.StorageUploadSession
	require.NoError(t, db.Where("owner_id = ?", 300).First(&sess).Error)
	assert.Equal(t, "public-uploads", sess.Bucket)
}

// TestGenerateUploadURL_VisibilityPrivateKeepsBucket verifies the default
// (UNSPECIFIED/PRIVATE) path is untouched: caller bucket wins.
func TestGenerateUploadURL_VisibilityPrivateKeepsBucket(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, err := svc.GenerateUploadURL(context.Background(), &storagev1.GenerateUploadURLRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 301},
		Filename:    "doc.pdf",
		Md5:         "10000000000000000000000000000002",
		Size:        16,
		ContentType: "application/pdf",
		Visibility:  storagev1.Visibility_VISIBILITY_PRIVATE,
	})
	require.NoError(t, err)
	require.Contains(t, resp.GetUploadUrl(), "uploads")
}

// TestGenerateUploadURL_VisibilityPublicWithoutPublicBucket verifies the
// fail-closed behavior: PUBLIC with no public bucket configured is a
// bad-request, not a silent private placement.
func TestGenerateUploadURL_VisibilityPublicWithoutPublicBucket(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})
	// settings live on the registry now; clear the public bucket there
	svc.registry.SetSettings("uploads", "")

	_, err := svc.GenerateUploadURL(context.Background(), &storagev1.GenerateUploadURLRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 302},
		Filename:    "avatar.png",
		Md5:         "10000000000000000000000000000003",
		Size:        16,
		ContentType: "image/png",
		Visibility:  storagev1.Visibility_VISIBILITY_PUBLIC,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "public bucket")
}
