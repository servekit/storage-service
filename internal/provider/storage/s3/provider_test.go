package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

const (
	testBucket    = "test-bucket"
	testRegion    = "us-east-1"
	testAccessKey = "minioadmin"
	testSecretKey = "minioadmin"
)

// setupMinIO starts a MinIO testcontainer and creates a test bucket.
// It returns the provider, the container (for cleanup), and the endpoint.
func setupMinIO(t *testing.T) *S3Provider {
	t.Helper()

	ctx := context.Background()

	mc, err := minio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z",
		minio.WithUsername(testAccessKey),
		minio.WithPassword(testSecretKey),
	)
	require.NoError(t, err, "failed to start MinIO container")

	endpoint, err := mc.ConnectionString(ctx)
	require.NoError(t, err, "failed to get MinIO connection string")

	provider, err := NewS3Provider(endpoint, testRegion, testAccessKey, testSecretKey, "")
	require.NoError(t, err, "failed to create S3Provider")

	// Create the test bucket.
	_, err = provider.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(testBucket)})
	require.NoError(t, err, "failed to create test bucket")

	t.Cleanup(func() {
		_ = mc.Terminate(ctx)
	})

	return provider
}

func TestS3Provider_PutAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	content := []byte("hello world")
	key := "testdir/hello.txt"

	// Put object.
	err := provider.PutObject(ctx, testBucket, key, bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err, "PutObject failed")

	// Get object.
	rc, err := provider.GetObject(ctx, testBucket, key)
	require.NoError(t, err, "GetObject failed")
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err, "ReadAll failed")
	assert.Equal(t, content, got, "object content mismatch")
}

func TestS3Provider_PutObject_WithContentType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	content := []byte("{\"ok\":true}")
	key := "data.json"

	err := provider.PutObject(ctx, testBucket, key, bytes.NewReader(content), int64(len(content)),
		types.WithContentType("application/json"),
	)
	require.NoError(t, err, "PutObject with content type failed")

	info, err := provider.HeadObject(ctx, testBucket, key)
	require.NoError(t, err, "HeadObject failed")
	assert.Equal(t, "application/json", info.ContentType)
}

func TestS3Provider_HeadObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	content := []byte("head test content")
	key := "head-test.txt"

	err := provider.PutObject(ctx, testBucket, key, bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err, "PutObject failed")

	info, err := provider.HeadObject(ctx, testBucket, key)
	require.NoError(t, err, "HeadObject failed")

	assert.Equal(t, key, info.Key)
	assert.Equal(t, int64(len(content)), info.Size)
	assert.NotEmpty(t, info.ETag)
	assert.False(t, info.LastModified.IsZero(), "LastModified should not be zero")
}

func TestS3Provider_DeleteObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	content := []byte("to be deleted")
	key := "delete-me.txt"

	err := provider.PutObject(ctx, testBucket, key, bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err, "PutObject failed")

	// Delete.
	err = provider.DeleteObject(ctx, testBucket, key)
	require.NoError(t, err, "DeleteObject failed")

	// Verify deletion: GetObject should fail.
	_, err = provider.GetObject(ctx, testBucket, key)
	assert.Error(t, err, "expected error after deletion")
}

func TestS3Provider_ListObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	prefix := "list-test/"
	keys := []string{
		prefix + "a.txt",
		prefix + "b.txt",
		prefix + "subdir/c.txt",
	}

	for _, k := range keys {
		err := provider.PutObject(ctx, testBucket, k, strings.NewReader("content"), 7)
		require.NoError(t, err, fmt.Sprintf("PutObject %s failed", k))
	}

	objects, err := provider.ListObjects(ctx, testBucket, prefix)
	require.NoError(t, err, "ListObjects failed")

	gotKeys := make([]string, len(objects))
	for i, o := range objects {
		gotKeys[i] = o.Key
	}
	assert.ElementsMatch(t, keys, gotKeys, "listed keys mismatch")
}

func TestS3Provider_PresignGetObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	content := []byte("presign test")
	key := "presign-test.txt"

	err := provider.PutObject(ctx, testBucket, key, bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err, "PutObject failed")

	url, err := provider.PresignGetObject(ctx, testBucket, key, 15*time.Minute)
	require.NoError(t, err, "PresignGetObject failed")
	assert.NotEmpty(t, url, "presigned URL should not be empty")
	assert.Contains(t, url, key, "presigned URL should contain the object key")
}

func TestS3Provider_PresignPutObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	provider := setupMinIO(t)
	ctx := context.Background()

	key := "presign-put-test.txt"

	url, headers, err := provider.PresignPutObject(ctx, testBucket, key, 15*time.Minute)
	require.NoError(t, err, "PresignPutObject failed")
	assert.NotEmpty(t, url, "presigned URL should not be empty")
	assert.NotNil(t, headers, "signed headers should not be nil")
}

// TestS3Provider_AliyunClient removed: AliyunClient() was deleted when the
// imgproc processor was refactored to depend on Provider.PresignGetObject
// directly. S3 no longer needs the stub method.

// --- internal helpers ---
