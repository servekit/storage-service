package tencent

import (
	"encoding/base64"
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

func TestBuildTencentStyle_ResizeBothDims_Lfit(t *testing.T) {
	ops := []types.Op{{Type: types.OpResize, Width: 200, Height: 150}}
	got := buildTencentStyle(ops)
	want := "thumbnail/200x150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_ResizeFill(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
	}
	got := buildTencentStyle(ops)
	want := "thumbnail/200x150!"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_ResizeWidthOnly(t *testing.T) {
	ops := []types.Op{{Type: types.OpResize, Width: 300}}
	got := buildTencentStyle(ops)
	want := "thumbnail/300x"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_ResizeHeightOnly(t *testing.T) {
	ops := []types.Op{{Type: types.OpResize, Height: 300}}
	got := buildTencentStyle(ops)
	want := "thumbnail/x300"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_ResizeZeroDims(t *testing.T) {
	// Zero dims for resize yields no segment (no-op resize is meaningless and
	// would produce a malformed "thumbnail/" segment otherwise).
	ops := []types.Op{{Type: types.OpResize}}
	got := buildTencentStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (resize with zero dims is a no-op)", got)
	}
}

func TestBuildTencentStyle_FormatQualityPipeline(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
	}
	got := buildTencentStyle(ops)
	want := "thumbnail/200x150/format/webp/quality/80"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_CropBothDims(t *testing.T) {
	ops := []types.Op{{Type: types.OpCrop, Width: 100, Height: 100}}
	got := buildTencentStyle(ops)
	want := "crop/100x100"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_CropNoDims(t *testing.T) {
	// crop with no dims is technically a no-op but still well-formed in
	// imageMogr2 (the cloud treats it as "crop with default size"); we emit
	// the bare "crop" segment to match the input shape.
	ops := []types.Op{{Type: types.OpCrop}}
	got := buildTencentStyle(ops)
	want := "crop"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_Rotate(t *testing.T) {
	ops := []types.Op{{Type: types.OpRotate, RotateDegrees: 90}}
	got := buildTencentStyle(ops)
	want := "rotate/90"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_Watermark(t *testing.T) {
	text := "hello"
	ops := []types.Op{{Type: types.OpWatermark, WatermarkText: text}}
	got := buildTencentStyle(ops)
	encoded := base64.URLEncoding.EncodeToString([]byte(text))
	want := "watermark/2/text/" + encoded
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_Blur(t *testing.T) {
	ops := []types.Op{{Type: types.OpBlur, BlurRadius: 2, BlurSigma: 5}}
	got := buildTencentStyle(ops)
	want := "blur/2x5"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_BlurOmittedWhenZero(t *testing.T) {
	// Both radius and sigma == 0 is a no-op; emit nothing rather than a
	// malformed "blur/0x0" segment.
	ops := []types.Op{{Type: types.OpBlur}}
	got := buildTencentStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (blur with zero radius/sigma is a no-op)", got)
	}
}

func TestBuildTencentStyle_Sharpen(t *testing.T) {
	ops := []types.Op{{Type: types.OpSharpen, SharpenAmount: 50}}
	got := buildTencentStyle(ops)
	want := "sharpen/50"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_Progressive(t *testing.T) {
	ops := []types.Op{{Type: types.OpProgressive, Progressive: true}}
	got := buildTencentStyle(ops)
	want := "interlace/1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_AutoOrient(t *testing.T) {
	ops := []types.Op{{Type: types.OpAutoOrient, AutoOrient: true}}
	got := buildTencentStyle(ops)
	want := "auto-orient"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_StripMetadata(t *testing.T) {
	ops := []types.Op{{Type: types.OpStripMetadata, StripMetadata: true}}
	got := buildTencentStyle(ops)
	want := "strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_HeicFallsBackToWebp(t *testing.T) {
	ops := []types.Op{{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_HEIC}}
	got := buildTencentStyle(ops)
	want := "format/webp"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_AvifFallsBackToWebp(t *testing.T) {
	ops := []types.Op{{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_AVIF}}
	got := buildTencentStyle(ops)
	want := "format/webp"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildTencentStyle_BooleanTogglesOff(t *testing.T) {
	// All boolean toggles with their flag zero must produce no segment so
	// empty/unset ops are dropped rather than emitting a no-op segment.
	ops := []types.Op{
		{Type: types.OpProgressive},
		{Type: types.OpAutoOrient},
		{Type: types.OpStripMetadata},
	}
	got := buildTencentStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (all toggles off)", got)
	}
}

// TestBuildTencentStyle_EmptyOps verifies empty input returns empty string.
func TestBuildTencentStyle_EmptyOps(t *testing.T) {
	got := buildTencentStyle(nil)
	if got != "" {
		t.Errorf("got %q, want empty for nil ops", got)
	}
	got = buildTencentStyle([]types.Op{})
	if got != "" {
		t.Errorf("got %q, want empty for empty ops slice", got)
	}
}

// TestBuildTencentStyle_ThumbnailPipeline verifies a realistic thumbnail
// pipeline: auto-orient (fix mobile rotation) -> resize -> sharpen (recover
// detail) -> progressive (web optimize) -> strip (privacy). Order matters: the
// cloud applies segments left-to-right.
func TestBuildTencentStyle_ThumbnailPipeline(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
		{Type: types.OpResize, Width: 200, Height: 200, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
		{Type: types.OpSharpen, SharpenAmount: 30},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
		{Type: types.OpProgressive, Progressive: true},
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildTencentStyle(ops)
	want := "auto-orient/thumbnail/200x200!/sharpen/30/format/webp/quality/80/interlace/1/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
