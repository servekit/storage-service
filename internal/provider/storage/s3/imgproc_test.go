package s3

import (
	"testing"

	"github.com/stretchr/testify/assert"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// TestBuildS3ProcessStyle_AlwaysEmpty locks in the stub's current contract:
// returns empty string for any input. This is the canary that forces a code
// review when S3 image processing is wired up — the test must be updated to
// reflect the new return value, and PresignGetObject's call site updated to
// apply the result.
func TestBuildS3ProcessStyle_AlwaysEmpty(t *testing.T) {
	cases := []struct {
		name string
		ops  []types.Op
	}{
		{"empty", nil},
		{"resize only", []types.Op{{Type: types.OpResize, Width: 100, Height: 100}}},
		{"pipeline", []types.Op{
			{Type: types.OpAutoOrient, AutoOrient: true},
			{Type: types.OpResize, Width: 200, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
			{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, "", buildS3ProcessStyle(tc.ops),
				"stub must return empty until a real S3 integration is wired up")
		})
	}
}
