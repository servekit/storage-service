package aliyun

import (
	"encoding/base64"
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

func TestBuildOssProcessStyle_ResizeOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
	}
	got := buildOssProcessStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_ResizeWithMode(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
	}
	got := buildOssProcessStyle(ops)
	want := "image/resize,m_fill,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_ResizeWidthOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 300},
	}
	got := buildOssProcessStyle(ops)
	want := "image/resize,m_lfit,w_300"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_ResizeFormatQuality(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
	}
	got := buildOssProcessStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150/image/format,webp/image/quality,q_80"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_Crop(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpCrop, Width: 100, Height: 100},
	}
	got := buildOssProcessStyle(ops)
	want := "image/crop,w_100,h_100"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_Rotate(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpRotate, RotateDegrees: 90},
	}
	got := buildOssProcessStyle(ops)
	want := "image/rotate,90"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_Watermark(t *testing.T) {
	text := "hello"
	ops := []types.Op{
		{Type: types.OpWatermark, WatermarkText: text},
	}
	got := buildOssProcessStyle(ops)
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	want := "image/watermark,text_" + encoded
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_AllOps(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 800, Height: 600, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_PAD},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_JPG},
		{Type: types.OpQuality, Quality: 90},
		{Type: types.OpRotate, RotateDegrees: 180},
	}
	got := buildOssProcessStyle(ops)

	want := "image/resize,m_pad,w_800,h_600/image/format,jpg/image/quality,q_90/image/rotate,180"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildOssProcessStyle_CropNoDimensions(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpCrop},
	}
	got := buildOssProcessStyle(ops)
	want := "image/crop"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_Blur covers OpBlur with both radius and sigma
// populated. Order is fixed (r before s) to match Aliyun docs.
func TestBuildOssProcessStyle_Blur(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpBlur, BlurRadius: 2, BlurSigma: 5},
	}
	got := buildOssProcessStyle(ops)
	want := "image/blur,r_2,s_5"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_Sharpen covers OpSharpen — only emitted when amount
// > 0, since 0 is the no-op zero value.
func TestBuildOssProcessStyle_Sharpen(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpSharpen, SharpenAmount: 50},
	}
	got := buildOssProcessStyle(ops)
	want := "image/sharpen,p_50"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_Progressive verifies OpProgressive emits interlace,1
// when toggled on. Off → no segment (avoids emitting interlace,0 which Aliyun
// would interpret as "force non-progressive", changing behavior for already-
// progressive inputs).
func TestBuildOssProcessStyle_Progressive(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive, Progressive: true},
	}
	got := buildOssProcessStyle(ops)
	want := "image/interlace,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_AutoOrient covers the EXIF-rotation fix. Critical
// for mobile camera uploads which store rotated sensor data + EXIF tag.
func TestBuildOssProcessStyle_AutoOrient(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
	}
	got := buildOssProcessStyle(ops)
	want := "image/auto-orient,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_StripMetadata verifies the strip segment for
// EXIF/IPTC/XMP removal (privacy + size optimization).
func TestBuildOssProcessStyle_StripMetadata(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildOssProcessStyle(ops)
	want := "image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildOssProcessStyle_BooleanTogglesOff verifies that Op{Progressive,
// AutoOrient, StripMetadata} produce no segment when their bool is false —
// callers should not see a difference between "omitted" and "explicitly off".
func TestBuildOssProcessStyle_BooleanTogglesOff(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive},
		{Type: types.OpAutoOrient},
		{Type: types.OpStripMetadata},
	}
	got := buildOssProcessStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (all toggles off)", got)
	}
}

// TestBuildOssProcessStyle_ThumbnailPipeline verifies a realistic thumbnail
// pipeline: auto-orient (fix mobile rotation) → resize → sharpen (recover
// detail) → progressive (web optimize) → strip (privacy). Order matters: the
// cloud applies segments left-to-right.
func TestBuildOssProcessStyle_ThumbnailPipeline(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
		{Type: types.OpResize, Width: 200, Height: 200, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
		{Type: types.OpSharpen, SharpenAmount: 30},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
		{Type: types.OpProgressive, Progressive: true},
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildOssProcessStyle(ops)
	want := "image/auto-orient,1/image/resize,m_fill,w_200,h_200/image/sharpen,p_30/image/format,webp/image/quality,q_80/image/interlace,1/image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
