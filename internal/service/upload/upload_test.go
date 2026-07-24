package upload

import (
	"context"
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

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
	// Object key shape is "<keyPrefix>/<md5[:2]>/<md5>". setup_test.go uses
	// KeyPrefix "uploads/", so the expected key is "uploads/00/<md5>".
	assert.Equal(t, "uploads/00/"+md5, token.GetObjectKey(),
		"object_key must be the full sharded path, not just the keyPrefix")
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
