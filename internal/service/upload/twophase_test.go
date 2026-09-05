package upload

import (
	"context"
	"io"
	"testing"
	"time"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/go-common/redisx"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/sts"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// owner100/owner200 are distinct upload owners used across the two-phase
// tests (victim vs attacker scenarios).
var (
	owner100 = &storagev1.Owner{OwnerType: 1, OwnerId: 100}
	owner200 = &storagev1.Owner{OwnerType: 1, OwnerId: 200}
)

const victimMD5 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// requestUploadURL drives GenerateUploadURL and returns the response plus the
// persisted session. Fails the test on any error or instant-dedup hit.
func requestUploadURL(t *testing.T, svc *Service, owner *storagev1.Owner, md5 string, size int64) (*storagev1.GenerateUploadURLResponse, *models.StorageUploadSession) {
	t.Helper()
	resp, err := svc.GenerateUploadURL(context.Background(), &storagev1.GenerateUploadURLRequest{
		Owner: owner, Md5: md5, Size: size, Filename: "f-" + md5 + ".bin",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant(), "expected a credential response, not an instant dedup hit")
	token, err := VerifyTokenForTest(resp.GetUploadToken(), testSecret, owner.GetOwnerId(), int32(owner.GetOwnerType()))
	require.NoError(t, err)
	sess, err := dal.GetUploadSessionByID(context.Background(), svc.db, token.SessionID)
	require.NoError(t, err)
	return resp, sess
}

// confirm drives ConfirmUpload with the given owner and raw upload token.
func confirm(t *testing.T, svc *Service, owner *storagev1.Owner, uploadToken string) (*storagev1.ConfirmUploadResponse, error) {
	t.Helper()
	return svc.ConfirmUpload(context.Background(), &storagev1.ConfirmUploadRequest{
		Owner: owner, UploadToken: uploadToken,
	})
}

// objectByFile resolves the StorageObject row behind a confirmed file.
func objectByFile(t *testing.T, svc *Service, fileID int64) *models.StorageObject {
	t.Helper()
	file, err := dal.GetFileByID(context.Background(), svc.db, fileID)
	require.NoError(t, err)
	obj, err := dal.GetObjectByID(context.Background(), svc.db, file.ObjectID)
	require.NoError(t, err)
	return obj
}

// readFakeObject fetches an object's bytes from the fake provider.
func readFakeObject(t *testing.T, fp readerGetter, bucket, key string) []byte {
	t.Helper()
	rc, err := fp.GetObject(context.Background(), bucket, key)
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	return body
}

// readerGetter is the GetObject slice of the fake provider.
type readerGetter interface {
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
}

// TestGenerateUploadURL_IssuesUnguessableStagingKey verifies the two-phase
// invariant at issue time: the credential (presigned URL / object key) targets
// an unguessable owner-scoped staging key, never the derivable
// content-addressed final key. This is what makes confirmed objects immune to
// overwrite by another user who knows the MD5.
func TestGenerateUploadURL_IssuesUnguessableStagingKey(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, sess := requestUploadURL(t, svc, owner100, victimMD5, 4)

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	staging := resp.GetObjectKey()

	assert.True(t, conv.IsSandboxObjectKey(staging, "uploads/", 1, 100),
		"issued key %q must live under the owner's staging sandbox", staging)
	assert.NotEqual(t, finalKey, staging, "the derivable final key must never be issued")
	assert.Equal(t, staging, sess.ObjectKey, "session row must pin the staging key")
	assert.Contains(t, resp.GetUploadUrl(), staging,
		"presigned URL must target the staging key, got %q", resp.GetUploadUrl())

	// Different owners must not share a staging key for identical content.
	resp2, sess2 := requestUploadURL(t, svc, owner200, victimMD5, 4)
	assert.NotEqual(t, staging, resp2.GetObjectKey(), "staging keys must be per-session random")
	assert.NotEqual(t, sess.ObjectKey, sess2.ObjectKey)
}

// TestConfirmUpload_TwoPhaseLandsAtFinalKey drives the full happy path: PUT to
// the staging key, confirm, then assert (a) the object row points at the
// content-addressed final key, (b) the final key holds the bytes server-side,
// (c) the staging object is reclaimed.
func TestConfirmUpload_TwoPhaseLandsAtFinalKey(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, _ := requestUploadURL(t, svc, owner100, victimMD5, 4)
	staging := resp.GetObjectKey()
	fp.PutObjectWithMD5(context.Background(), "uploads", staging, []byte("data"), "text/plain", victimMD5)

	confirmed, err := confirm(t, svc, owner100, resp.GetUploadToken())
	require.NoError(t, err)

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	obj := objectByFile(t, svc, confirmed.GetFileId())
	assert.Equal(t, finalKey, obj.ObjectKey, "object row must record the final key")
	assert.False(t, fp.ObjectExists("uploads", staging), "staging object must be reclaimed after confirm")

	finalInfo, err := fp.HeadObject(context.Background(), "uploads", finalKey)
	require.NoError(t, err)
	assert.Equal(t, int64(4), finalInfo.Size)
	assert.Equal(t, `"`+victimMD5+`"`, finalInfo.ETag, "final key must hold the verified bytes")
}

// TestConfirmUpload_OverwritePollutionRegression is the core security
// regression for the content-address overwrite bug: a second user who knows
// the victim's MD5 must be unable to replace the victim's confirmed bytes.
// Pre-fix, the attacker could PUT to the victim's final key directly (the
// presigned URL locked a derivable key). Post-fix, every credential targets
// the requester's own unguessable staging key — the attacker's malicious
// bytes can never land at the final key, and even a matching-MD5 upload is
// byte-identical by definition.
func TestConfirmUpload_OverwritePollutionRegression(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	// Attacker requests an upload credential for the victim's (known) md5
	// FIRST — before any confirmed object row exists, so no DB dedup short-
	// circuit. They receive their own unguessable staging key.
	attackResp, _ := requestUploadURL(t, svc, owner200, victimMD5, 4)
	attackKey := attackResp.GetObjectKey()

	// Victim uploads and confirms the original bytes.
	victimResp, _ := requestUploadURL(t, svc, owner100, victimMD5, 4)
	require.NotEqual(t, attackKey, victimResp.GetObjectKey(),
		"different owners must never share a staging key")
	fp.PutObjectWithMD5(context.Background(), "uploads", victimResp.GetObjectKey(),
		[]byte("good"), "text/plain", victimMD5)
	_, err := confirm(t, svc, owner100, victimResp.GetUploadToken())
	require.NoError(t, err)

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	require.True(t, fp.ObjectExists("uploads", finalKey), "victim's final object must exist")
	require.NotEqual(t, finalKey, attackKey, "attacker must not receive the final key")

	// Attacker uploads different bytes to their staging key and tries to
	// confirm — the md5 check rejects the tampered staging bytes…
	fp.PutObjectWithMD5(context.Background(), "uploads", attackKey,
		[]byte("evil"), "text/plain", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	_, err = confirm(t, svc, owner200, attackResp.GetUploadToken())
	require.Error(t, err, "confirm must reject md5-mismatched staging bytes")

	// …and the victim's final object is untouched throughout. There is no key
	// the attacker could ever PUT to that aliases the victim's confirmed
	// bytes — the pre-fix presigned-URL-for-derivable-key path is gone.
	assert.Equal(t, []byte("good"), readFakeObject(t, fp, "uploads", finalKey),
		"victim's confirmed bytes must survive the attacker's upload")
}

// TestConfirmUpload_DedupSkipsCopyWhenFinalExists pins the global-dedup
// semantics of ensureFinalObject: when the final key already holds an object
// with a matching MD5 ETag, the copy is skipped and the existing bytes stay
// authoritative.
func TestConfirmUpload_DedupSkipsCopyWhenFinalExists(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	// Pre-existing final object whose ETag matches the md5 but whose data is
	// distinguishable — if confirm copied over it, the data would change.
	fp.PutObjectWithMD5(context.Background(), "uploads", finalKey,
		[]byte("original"), "text/plain", victimMD5)

	resp, _ := requestUploadURL(t, svc, owner100, victimMD5, int64(len("same-md5-different-bytes")))
	fp.PutObjectWithMD5(context.Background(), "uploads", resp.GetObjectKey(),
		[]byte("same-md5-different-bytes"), "text/plain", victimMD5)
	_, err := confirm(t, svc, owner100, resp.GetUploadToken())
	require.NoError(t, err)

	assert.Equal(t, []byte("original"), readFakeObject(t, fp, "uploads", finalKey),
		"existing final object with matching md5 etag must not be re-copied (dedup preserved)")
}

// TestConfirmUpload_HealsCorruptFinalObject covers the opposite branch: a
// final key whose stored ETag disagrees with its own content address is
// corrupt by definition — confirm must overwrite it with freshly verified
// bytes rather than trusting the polluted object.
func TestConfirmUpload_HealsCorruptFinalObject(t *testing.T) {
	svc, fp, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	// Legacy pollution: bytes that don't hash to the key's address.
	fp.PutObjectWithMD5(context.Background(), "uploads", finalKey,
		[]byte("polluted"), "text/plain", "cccccccccccccccccccccccccccccccc")

	resp, _ := requestUploadURL(t, svc, owner100, victimMD5, 4)
	fp.PutObjectWithMD5(context.Background(), "uploads", resp.GetObjectKey(),
		[]byte("good"), "text/plain", victimMD5)
	_, err := confirm(t, svc, owner100, resp.GetUploadToken())
	require.NoError(t, err)

	assert.Equal(t, []byte("good"), readFakeObject(t, fp, "uploads", finalKey),
		"corrupt final object must be healed by the copy")
}

// TestConfirmUpload_LegacySessionFinalKeyCompat verifies in-flight sessions
// minted before the two-phase flow (ObjectKey == final content-addressed key)
// still confirm without a copy — source and destination are the same key.
func TestConfirmUpload_LegacySessionFinalKeyCompat(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})

	finalKey := conv.ObjectKeyFromMD5("uploads/", victimMD5)
	legacy := &models.StorageUploadSession{
		ID: 9001, OwnerType: 1, OwnerID: 100, Bucket: "uploads", ObjectKey: finalKey,
		MD5: victimMD5, Size: 6, Filename: "legacy.bin", ContentType: "text/plain",
		Vendor: 3, Status: int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	require.NoError(t, dal.CreateUploadSession(context.Background(), db, legacy))
	fp.PutObjectWithMD5(context.Background(), "uploads", finalKey,
		[]byte("legacy"), "text/plain", victimMD5)

	token := &uploadToken{
		SessionID: legacy.ID, OwnerID: 100, OwnerType: 1,
		MD5: victimMD5, Size: 6, ContentType: "text/plain",
		Bucket: "uploads", Vendor: 3,
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}
	tokenStr, err := SignTokenForTest(token, testSecret)
	require.NoError(t, err)

	confirmed, err := confirm(t, svc, owner100, tokenStr)
	require.NoError(t, err, "legacy session must confirm without a staging copy")

	obj := objectByFile(t, svc, confirmed.GetFileId())
	assert.Equal(t, finalKey, obj.ObjectKey, "legacy object row keeps the content-addressed key")
	assert.True(t, fp.ObjectExists("uploads", finalKey), "legacy object must survive")
}

// --- STS policy narrowing ---

// installCapturingIssuer swaps the service's STS issuer for one that records
// every minted policy and returns a canned credential. Returns the shared
// slice the policies accumulate into.
func installCapturingIssuer(t *testing.T, svc *Service) *[]*storage.STSPolicy {
	t.Helper()
	rdb := redisx.NewTestClient(t)
	captured := &[]*storage.STSPolicy{}
	SetSTS(svc, rdb, stsCaptureIssuer{captured}, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})
	return captured
}

// stsCaptureIssuer adapts a policy-recording function to sts.Issuer.
type stsCaptureIssuer struct{ captured *[]*storage.STSPolicy }

func (c stsCaptureIssuer) Issue(_ context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	*c.captured = append(*c.captured, policy)
	return &storage.STSCredential{
		AccessKey: "ak-cap", SecretKey: "sk-cap", SecurityToken: "st-cap",
		Endpoint: "http://fake-endpoint", Bucket: policy.Bucket,
		ObjectKeyPrefix: policy.KeyPrefix, ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// Compile-time assertion that stsCaptureIssuer satisfies the issuer contract.
var _ sts.Issuer = stsCaptureIssuer{}

// TestGetSTSCredential_PolicyScopedToOwnerSandbox verifies the STS credential
// is scoped to the owner's staging sandbox (not the bucket's whole key
// prefix), closing the bucket/prefix-wide write window of the old policy.
func TestGetSTSCredential_PolicyScopedToOwnerSandbox(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	captured := installCapturingIssuer(t, svc)

	_, err := svc.GetSTSCredential(context.Background(), &storagev1.GetSTSCredentialRequest{
		Owner: owner100, Bucket: "uploads",
		Md5: victimMD5, MaxSize: 4, ContentType: "text/plain", Filename: "a.bin",
	})
	require.NoError(t, err)
	require.Len(t, *captured, 1)

	p := (*captured)[0]
	assert.Equal(t, "uploads/tmp/1-100/", p.KeyPrefix,
		"STS policy must be scoped to the owner staging sandbox")
	assert.Empty(t, p.AllowedExtensions,
		"extension resources cannot match extension-less staging keys; enforcement stays at issue time")
	assert.True(t, p.EnforceHTTPS && p.LockObjectACL && p.DenyPutObjectACL,
		"hardening flags must stay enabled")
}

// TestBatchGetSTSCredential_SharedPolicyScopedToSandbox verifies the batch
// path's SHARED credential carries the same sandbox scope — the STS cache is
// keyed on owner+vendor+bucket only, so a broader batch policy would poison
// the single-file path's cache slot with a bucket-wide credential.
func TestBatchGetSTSCredential_SharedPolicyScopedToSandbox(t *testing.T) {
	svc, _, _ := setupUploadServiceWithFakeProvider(t, noopHost{})

	captured := installCapturingIssuer(t, svc)

	_, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner: owner100,
		Files: []*storagev1.UploadFileMeta{
			{Filename: "x.bin", Md5: victimMD5, Size: 4, ContentType: "application/octet-stream"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, *captured)
	for _, p := range *captured {
		assert.Equal(t, "uploads/tmp/1-100/", p.KeyPrefix,
			"every minted policy (shared + per-file) must use the owner sandbox")
	}
}
