package tencent

import (
	"encoding/base64"
	"fmt"
	"strings"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// buildTencentStyle translates typed ops into Tencent Cloud imageMogr2 syntax.
// imageMogr2 differs from Aliyun x-oss-process: there is no per-op "image/"
// prefix and segments within a single op are joined with "/" (e.g.
// "thumbnail/100x100"). Multiple ops are concatenated with "/" too, so the
// full string looks like "thumbnail/100x100/format/webp/quality/80".
//
// Kept as a package-level helper (not a method on TencentProvider) so it can
// be unit-tested in isolation. Pure function — no Provider state required.
//
// Spec: https://cloud.tencent.com/document/product/460/36540
func buildTencentStyle(ops []types.Op) string {
	var parts []string
	for _, op := range ops {
		switch op.Type {
		case types.OpResize:
			// imageMogr2 thumbnail uses "WxH" with a trailing mode suffix.
			//   !       -> force crop to exact WxH (FILL)
			//   (none)  -> lfit, fits within WxH preserving aspect ratio
			// Tencent docs: thumbnail/<Width>x<Height><Mode>
			mode := tencentResizeSuffix(op.ResizeMode)
			dims := tencentResizeDims(op.Width, op.Height)
			if dims != "" {
				parts = append(parts, "thumbnail/"+dims+mode)
			}
		case types.OpFormat:
			parts = append(parts, "format/"+tencentFormat(op.Format))
		case types.OpQuality:
			parts = append(parts, fmt.Sprintf("quality/%d", op.Quality))
		case types.OpCrop:
			// imageMogr2 crop/<W>x<H>. Use cut (top-left) by default.
			dims := tencentResizeDims(op.Width, op.Height)
			if dims == "" {
				parts = append(parts, "crop")
			} else {
				parts = append(parts, "crop/"+dims)
			}
		case types.OpRotate:
			parts = append(parts, fmt.Sprintf("rotate/%d", op.RotateDegrees))
		case types.OpWatermark:
			// imageMogr2 watermark text uses base64-urlsafe encoding under the
			// "text" param: watermark/2/text/<base64>. Advanced fields (color,
			// position, font) are out of scope for the Op struct.
			encoded := tencentWatermarkEncode(op.WatermarkText)
			parts = append(parts, "watermark/2/text/"+encoded)
		case types.OpBlur:
			// imageMogr2 blur/<radius>x<sigma>
			if op.BlurRadius > 0 || op.BlurSigma > 0 {
				parts = append(parts, fmt.Sprintf("blur/%dx%d", op.BlurRadius, op.BlurSigma))
			}
		case types.OpSharpen:
			// imageMogr2 sharpen/<value> where value is 0-100 (sharpen amount).
			if op.SharpenAmount > 0 {
				parts = append(parts, fmt.Sprintf("sharpen/%d", op.SharpenAmount))
			}
		case types.OpProgressive:
			if op.Progressive {
				parts = append(parts, "interlace/1")
			}
		case types.OpAutoOrient:
			if op.AutoOrient {
				parts = append(parts, "auto-orient")
			}
		case types.OpStripMetadata:
			if op.StripMetadata {
				parts = append(parts, "strip")
			}
		}
	}
	return strings.Join(parts, "/")
}

// --- internal helpers ---

// tencentResizeDims formats the WxH dimension string for thumbnail/crop in
// imageMogr2 syntax: "WxH", "Wx", "xH", or "" when both are zero.
func tencentResizeDims(width, height int) string {
	switch {
	case width > 0 && height > 0:
		return fmt.Sprintf("%dx%d", width, height)
	case width > 0:
		return fmt.Sprintf("%dx", width)
	case height > 0:
		return fmt.Sprintf("x%d", height)
	default:
		return ""
	}
}

// tencentResizeSuffix maps the proto ImageResizeMode to imageMogr2's thumbnail
// mode suffix. FILL -> "!" (force crop to exact WxH); PAD/UNSPECIFIED fall
// back to no suffix (limit / lfit) since imageMogr2 lacks a direct PAD
// equivalent (that requires the composite API, out of scope for this builder).
func tencentResizeSuffix(m storagev1.ImageResizeMode) string {
	switch m {
	case storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL:
		return "!"
	default:
		return ""
	}
}

// tencentFormat maps proto ImageFormat to imageMogr2 format value. imageMogr2
// supports jpg, png, webp, gif, bmp; heic/avif are NOT supported by imageMogr2
// (Tencent CI handles those via a different API). HEIC/AVIF fall back to webp
// to keep the request well-formed rather than returning a 400.
func tencentFormat(f storagev1.ImageFormat) string {
	switch f {
	case storagev1.ImageFormat_IMAGE_FORMAT_JPG:
		return "jpg"
	case storagev1.ImageFormat_IMAGE_FORMAT_PNG:
		return "png"
	case storagev1.ImageFormat_IMAGE_FORMAT_WEBP:
		return "webp"
	case storagev1.ImageFormat_IMAGE_FORMAT_GIF:
		return "gif"
	case storagev1.ImageFormat_IMAGE_FORMAT_BMP:
		return "bmp"
	case storagev1.ImageFormat_IMAGE_FORMAT_HEIC, storagev1.ImageFormat_IMAGE_FORMAT_AVIF:
		// imageMogr2 doesn't support heic/avif output. Fall back to webp so the
		// request still succeeds; callers wanting true heic should use Tencent
		// CI's live Picasso API (out of scope for this provider).
		return "webp"
	default:
		return "jpg"
	}
}

// tencentWatermarkEncode base64-urlsafe-encodes the watermark text per
// imageMogr2 spec (url-safe alphabet, standard padding).
func tencentWatermarkEncode(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}
