package huawei

import (
	"encoding/base64"
	"fmt"
	"strings"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// buildObsProcessStyle translates typed ops into Huawei OBS image process
// syntax. Each Op becomes one image/<action> segment; segments are joined
// with "/". Empty input returns empty string.
//
// Huawei OBS uses the same image/<action>,k_v syntax as Aliyun OSS, so
// this function is structurally identical to aliyun.buildOssProcessStyle.
// Kept as a separate package-level helper so the huawei package has no
// dependency on aliyun, and future Huawei-specific divergences (e.g.
// Huawei-only actions) have a clear home.
//
// The output is the value of the x-image-process query param on CDN URLs
// and the image process directive on PresignGetObject URLs.
func buildObsProcessStyle(ops []types.Op) string {
	var parts []string
	for _, op := range ops {
		switch op.Type {
		case types.OpResize:
			mode := huaweiResizeMode(op.ResizeMode)
			s := fmt.Sprintf("image/resize,m_%s", mode)
			if op.Width > 0 {
				s += fmt.Sprintf(",w_%d", op.Width)
			}
			if op.Height > 0 {
				s += fmt.Sprintf(",h_%d", op.Height)
			}
			parts = append(parts, s)
		case types.OpFormat:
			parts = append(parts, fmt.Sprintf("image/format,%s", huaweiFormat(op.Format)))
		case types.OpQuality:
			parts = append(parts, fmt.Sprintf("image/quality,q_%d", op.Quality))
		case types.OpCrop:
			s := "image/crop"
			if op.Width > 0 {
				s += fmt.Sprintf(",w_%d", op.Width)
			}
			if op.Height > 0 {
				s += fmt.Sprintf(",h_%d", op.Height)
			}
			parts = append(parts, s)
		case types.OpRotate:
			parts = append(parts, fmt.Sprintf("image/rotate,%d", op.RotateDegrees))
		case types.OpWatermark:
			encoded := base64.StdEncoding.EncodeToString([]byte(op.WatermarkText))
			parts = append(parts, fmt.Sprintf("image/watermark,text_%s", encoded))
		case types.OpBlur:
			s := "image/blur"
			if op.BlurRadius > 0 {
				s += fmt.Sprintf(",r_%d", op.BlurRadius)
			}
			if op.BlurSigma > 0 {
				s += fmt.Sprintf(",s_%d", op.BlurSigma)
			}
			parts = append(parts, s)
		case types.OpSharpen:
			if op.SharpenAmount > 0 {
				parts = append(parts, fmt.Sprintf("image/sharpen,p_%d", op.SharpenAmount))
			}
		case types.OpProgressive:
			if op.Progressive {
				parts = append(parts, "image/interlace,1")
			}
		case types.OpAutoOrient:
			if op.AutoOrient {
				parts = append(parts, "image/auto-orient,1")
			}
		case types.OpStripMetadata:
			if op.StripMetadata {
				parts = append(parts, "image/strip")
			}
		}
	}
	return strings.Join(parts, "/")
}

// --- internal helpers ---

func huaweiResizeMode(m storagev1.ImageResizeMode) string {
	switch m {
	case storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL:
		return "fill"
	case storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_PAD:
		return "pad"
	default:
		return "lfit"
	}
}

func huaweiFormat(f storagev1.ImageFormat) string {
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
	case storagev1.ImageFormat_IMAGE_FORMAT_HEIC:
		return "heic"
	case storagev1.ImageFormat_IMAGE_FORMAT_AVIF:
		return "avif"
	default:
		return "jpg"
	}
}
