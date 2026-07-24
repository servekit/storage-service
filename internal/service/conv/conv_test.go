package conv

import (
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"github.com/stretchr/testify/assert"
)

func TestACLToProto(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected storagev1.BucketACL
	}{
		{name: "private", input: "private", expected: storagev1.BucketACL_BUCKET_ACL_PRIVATE},
		{name: "public_read", input: "public_read", expected: storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ},
		{name: "public_read_write", input: "public_read_write", expected: storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE},
		{name: "unknown_returns_unspecified", input: "something_else", expected: storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED},
		{name: "empty_returns_unspecified", input: "", expected: storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED},
		{name: "case_sensitive_mismatch", input: "Private", expected: storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ACLToProto(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestObjectKeyFromMD5(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "p/ab/abcdef", ObjectKeyFromMD5("p/", "abcdef"))
	assert.Equal(t, "prefixab/abcdef", ObjectKeyFromMD5("prefix", "abcdef"))
	// Short MD5 (<2 chars) is returned inline without a slash.
	assert.Equal(t, "prefixa", ObjectKeyFromMD5("prefix", "a"))
	assert.Equal(t, "prefix", ObjectKeyFromMD5("prefix", ""))
}

func TestResolveBucket(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "explicit", ResolveBucket("explicit", "default"))
	assert.Equal(t, "default", ResolveBucket("", "default"))
}

func TestVendorToName(t *testing.T) {
	t.Parallel()

	// VENDOR_UNSPECIFIED and unknown values both map to "".
	assert.Empty(t, VendorToName(0))
	assert.Empty(t, VendorToName(999999))
	// 1 is the first real vendor; the enum name comes from the proto map.
	assert.NotEmpty(t, VendorToName(1))
}

func TestOwnerTypeToProto(t *testing.T) {
	t.Parallel()

	assert.Equal(t, storagev1.OwnerType(2), OwnerTypeToProto(2))
}

func TestProtoToImageOpNil(t *testing.T) {
	t.Parallel()

	op := ProtoToImageOp(nil)
	assert.Zero(t, op.Width)
}
