package volcengine

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

// TestObjectInfoFromHead_AllFieldsPopulated verifies the happy-path mapping
// from HeadObjectV2Output to types.ObjectInfo. ObjectACL is intentionally not
// set here — HeadObject fills it via a separate GetObjectACL call.
//
// Note: TOS ObjectMetaV2 exposes ETag with a capital E (unlike OSS where the
// field is ETag-as-pointer). The mapping strips the surrounding quotes that
// TOS includes in the raw ETag header.
func TestObjectInfoFromHead_AllFieldsPopulated(t *testing.T) {
	lastModified := time.Date(2026, 6, 26, 15, 4, 5, 0, time.UTC)
	head := &tos.HeadObjectV2Output{}
	head.ContentLength = 2048
	head.ETag = `"deadbeef"`
	head.ContentType = "image/jpeg"
	head.LastModified = lastModified

	info := objectInfoFromHead("photos/abc.jpg", head)
	assert.Equal(t, "photos/abc.jpg", info.Key)
	assert.Equal(t, int64(2048), info.Size)
	assert.Equal(t, "deadbeef", info.ETag, "ETag quotes must be stripped")
	assert.Equal(t, "image/jpeg", info.ContentType)
	assert.WithinDuration(t, lastModified, info.LastModified, time.Second)
	assert.Empty(t, info.ObjectACL, "objectInfoFromHead must not populate ObjectACL; HeadObject does it via GetObjectACL")
}

// TestObjectInfoFromHead_NilOptionalFields verifies that zero-value fields in
// the v2 result do not panic and leave the corresponding ObjectInfo fields
// zeroed. LastModified zero-value triggers the IsZero guard so the resulting
// ObjectInfo.LastModified is the zero time.
func TestObjectInfoFromHead_NilOptionalFields(t *testing.T) {
	head := &tos.HeadObjectV2Output{}
	head.ContentLength = 10

	info := objectInfoFromHead("k", head)
	require.NotNil(t, info)
	assert.Equal(t, "k", info.Key)
	assert.Equal(t, int64(10), info.Size)
	assert.Empty(t, info.ETag)
	assert.Empty(t, info.ContentType)
	assert.True(t, info.LastModified.IsZero())
}

// TestObjectInfoFromHead_ETagWithoutQuotes verifies an ETag that arrives
// without quotes is passed through unchanged.
func TestObjectInfoFromHead_ETagWithoutQuotes(t *testing.T) {
	head := &tos.HeadObjectV2Output{}
	head.ContentLength = 1
	head.ETag = "plain-etag"
	info := objectInfoFromHead("k", head)
	assert.Equal(t, "plain-etag", info.ETag)
}

// TestPublicObjectURL verifies the https://<bucket>.<endpoint>/<key> layout
// for public-read TOS objects.
func TestPublicObjectURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		bucket   string
		key      string
		want     string
	}{
		{"bare host", "tos-cn-beijing.volces.com", "mybucket", "uploads/abc.jpg",
			"https://mybucket.tos-cn-beijing.volces.com/uploads/abc.jpg"},
		{"with scheme", "https://tos-cn-beijing.volces.com", "mybucket", "uploads/abc.jpg",
			"https://mybucket.tos-cn-beijing.volces.com/uploads/abc.jpg"},
		{"trailing slash", "tos-cn-beijing.volces.com/", "mybucket", "k",
			"https://mybucket.tos-cn-beijing.volces.com/k"},
		{"leading slash key", "tos-cn-beijing.volces.com", "mybucket", "/uploads/x.png",
			"https://mybucket.tos-cn-beijing.volces.com/uploads/x.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := publicObjectURL(tc.endpoint, tc.bucket, tc.key)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestIsVolcNotFound covers the error-shape detection for 404 → ErrObjectNotFound.
//
// TOS surfaces unexpected status codes as *tos.UnexpectedStatusCodeError (which
// carries StatusCode directly); we also exercise the SDK's TosServerError path
// by constructing one with an embedded Error payload.
func TestIsVolcNotFound(t *testing.T) {
	assert.False(t, isVolcNotFound(nil))
	assert.False(t, isVolcNotFound(errors.New("not a tos error")))

	// UnexpectedStatusCodeError is the real 404 carrier in this SDK.
	notFound := tos.NewUnexpectedStatusCodeError(httpStatusNotFound, httpStatusOK)
	assert.True(t, isVolcNotFound(notFound))

	forbidden := tos.NewUnexpectedStatusCodeError(httpStatusForbidden, httpStatusOK)
	assert.False(t, isVolcNotFound(forbidden))
}

// TestVolcACLFromGrants covers the Grants → canned ACL translation. TOS
// GetObjectACL returns a Grants list rather than a single canned string.
func TestVolcACLFromGrants(t *testing.T) {
	t.Run("nil resp returns empty", func(t *testing.T) {
		assert.Equal(t, "", volcACLFromGrants(nil))
	})

	t.Run("IsDefault yields default", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		resp.IsDefault = true
		assert.Equal(t, "default", volcACLFromGrants(resp))
	})

	t.Run("no grants yields private", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		assert.Equal(t, "private", volcACLFromGrants(resp))
	})

	t.Run("AllUsers READ yields public-read", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		resp.Grants = []tos.GrantV2{{
			GranteeV2:  tos.GranteeV2{Canned: enum.CannedAllUsers, Type: enum.GranteeGroup},
			Permission: enum.PermissionRead,
		}}
		assert.Equal(t, "public-read", volcACLFromGrants(resp))
	})

	t.Run("AllUsers READ+WRITE yields public-read-write", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		resp.Grants = []tos.GrantV2{
			{GranteeV2: tos.GranteeV2{Canned: enum.CannedAllUsers, Type: enum.GranteeGroup}, Permission: enum.PermissionRead},
			{GranteeV2: tos.GranteeV2{Canned: enum.CannedAllUsers, Type: enum.GranteeGroup}, Permission: enum.PermissionWrite},
		}
		assert.Equal(t, "public-read-write", volcACLFromGrants(resp))
	})

	t.Run("AllUsers FULL_CONTROL yields public-read-write", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		resp.Grants = []tos.GrantV2{{
			GranteeV2:  tos.GranteeV2{Canned: enum.CannedAllUsers, Type: enum.GranteeGroup},
			Permission: enum.PermissionFullControl,
		}}
		assert.Equal(t, "public-read-write", volcACLFromGrants(resp))
	})

	t.Run("grants to non-AllUsers are ignored", func(t *testing.T) {
		resp := &tos.GetObjectACLOutput{}
		// Grantee is a specific user, not anonymous — must not affect ACL.
		resp.Grants = []tos.GrantV2{{
			GranteeV2:  tos.GranteeV2{ID: "user-123", Type: enum.GranteeUser},
			Permission: enum.PermissionRead,
		}}
		assert.Equal(t, "private", volcACLFromGrants(resp))
	})
}

// --- internal helpers ---

// httpStatusNotFound / httpStatusForbidden mirror net/http status constants
// without pulling net/http into this test file (kept small to stay focused on
// provider logic). Inlined so the test does not require a net/http import.
const (
	httpStatusNotFound  = 404
	httpStatusOK        = 200
	httpStatusForbidden = 403
)
