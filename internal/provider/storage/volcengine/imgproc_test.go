package volcengine

import (
	"encoding/base64"
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

func TestBuildVolcStyle_ResizeOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
	}
	got := buildVolcStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_ResizeWithMode(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
	}
	got := buildVolcStyle(ops)
	want := "image/resize,m_fill,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_ResizeWidthOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 300},
	}
	got := buildVolcStyle(ops)
	want := "image/resize,m_lfit,w_300"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_ResizeFormatQuality(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
	}
	got := buildVolcStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150/image/format,webp/image/quality,q_80"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Crop(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpCrop, Width: 100, Height: 100},
	}
	got := buildVolcStyle(ops)
	want := "image/crop,w_100,h_100"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Rotate(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpRotate, RotateDegrees: 90},
	}
	got := buildVolcStyle(ops)
	want := "image/rotate,90"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Watermark(t *testing.T) {
	text := "hello"
	ops := []types.Op{
		{Type: types.OpWatermark, WatermarkText: text},
	}
	got := buildVolcStyle(ops)
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	want := "image/watermark,text_" + encoded
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Blur(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpBlur, BlurRadius: 2, BlurSigma: 5},
	}
	got := buildVolcStyle(ops)
	want := "image/blur,r_2,s_5"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Sharpen(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpSharpen, SharpenAmount: 50},
	}
	got := buildVolcStyle(ops)
	want := "image/sharpen,p_50"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Progressive(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive, Progressive: true},
	}
	got := buildVolcStyle(ops)
	want := "image/interlace,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_AutoOrient(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
	}
	got := buildVolcStyle(ops)
	want := "image/auto-orient,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_StripMetadata(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildVolcStyle(ops)
	want := "image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_BooleanTogglesOff(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive},
		{Type: types.OpAutoOrient},
		{Type: types.OpStripMetadata},
	}
	got := buildVolcStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (all toggles off)", got)
	}
}

func TestBuildVolcStyle_ThumbnailPipeline(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
		{Type: types.OpResize, Width: 200, Height: 200, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
		{Type: types.OpSharpen, SharpenAmount: 30},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
		{Type: types.OpProgressive, Progressive: true},
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildVolcStyle(ops)
	want := "image/auto-orient,1/image/resize,m_fill,w_200,h_200/image/sharpen,p_30/image/format,webp/image/quality,q_80/image/interlace,1/image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildVolcStyle_Empty(t *testing.T) {
	got := buildVolcStyle(nil)
	if got != "" {
		t.Errorf("got %q, want empty for nil ops", got)
	}
}
