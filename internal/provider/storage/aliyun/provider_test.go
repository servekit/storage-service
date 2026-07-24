package aliyun

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
)

// TestObjectInfoFromHead_AllFieldsPopulated verifies the happy-path mapping
// from the v2 HeadObjectResult to types.ObjectInfo. ObjectACL is intentionally
// not set here — HeadObject fills it via a separate GetObjectAcl call.
func TestObjectInfoFromHead_AllFieldsPopulated(t *testing.T) {
	lastModified := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	head := &oss.HeadObjectResult{
		ContentLength: 2048,
		ETag:          oss.Ptr(`"deadbeef"`),
		ContentType:   oss.Ptr("image/jpeg"),
		LastModified:  oss.Ptr(lastModified),
	}

	info := objectInfoFromHead("photos/abc.jpg", head)
	assert.Equal(t, "photos/abc.jpg", info.Key)
	assert.Equal(t, int64(2048), info.Size)
	assert.Equal(t, "deadbeef", info.ETag, "ETag quotes must be stripped")
	assert.Equal(t, "image/jpeg", info.ContentType)
	assert.WithinDuration(t, lastModified, info.LastModified, time.Second)
	assert.Empty(t, info.ObjectACL, "objectInfoFromHead must not populate ObjectACL; HeadObject does it via GetObjectAcl")
}

// TestObjectInfoFromHead_NilOptionalFields verifies that nil pointer fields in
// the v2 result do not panic and leave the corresponding ObjectInfo fields
// zeroed.
func TestObjectInfoFromHead_NilOptionalFields(t *testing.T) {
	head := &oss.HeadObjectResult{
		ContentLength: 10,
		// ETag, ContentType, LastModified all nil
	}

	info := objectInfoFromHead("k", head)
	require.NotNil(t, info)
	assert.Equal(t, "k", info.Key)
	assert.Equal(t, int64(10), info.Size)
	assert.Empty(t, info.ETag)
	assert.Empty(t, info.ContentType)
	assert.True(t, info.LastModified.IsZero())
}

// TestObjectInfoFromHead_ETagWithoutQuotes verifies an ETag that arrives
// without quotes (some S3-compatible gateways do this) is passed through
// unchanged — strings.Trim only removes quotes when present.
func TestObjectInfoFromHead_ETagWithoutQuotes(t *testing.T) {
	head := &oss.HeadObjectResult{
		ContentLength: 1,
		ETag:          oss.Ptr("plain-etag"),
	}
	info := objectInfoFromHead("k", head)
	assert.Equal(t, "plain-etag", info.ETag)
}
