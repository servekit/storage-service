package types

import storagev1 "github.com/servekit/storage-service/gen/storage/v1"

// OpType defines the type of image operation. Vendor-agnostic — each provider
// translates []Op into its native syntax inside PresignGetObject.
type OpType int

// Op represents a single image processing operation. Only fields relevant to
// the active Type need to be set; others are ignored.
type Op struct {
	Type OpType

	// OpResize / OpCrop
	Width      int
	Height     int
	ResizeMode storagev1.ImageResizeMode

	// OpFormat / OpQuality
	Format  storagev1.ImageFormat
	Quality int

	// OpWatermark / OpRotate
	WatermarkText string
	RotateDegrees int

	// OpBlur: Gaussian blur. Radius controls pixel spread; Sigma controls
	// spread strength. Typical values: radius 0-50, sigma 0-50.
	BlurRadius int
	BlurSigma  int

	// OpSharpen: post-resize sharpening. Amount is 0-100 (percent).
	SharpenAmount int

	// OpProgressive / OpAutoOrient / OpStripMetadata: boolean toggles.
	Progressive   bool // progressive JPEG output (web optimization)
	AutoOrient    bool // apply EXIF orientation (mobile camera fix)
	StripMetadata bool // remove EXIF / IPTC / XMP metadata
}

const (
	// OpResize resizes an image.
	OpResize OpType = iota
	// OpCrop crops an image.
	OpCrop
	// OpQuality adjusts image quality.
	OpQuality
	// OpFormat converts image format.
	OpFormat
	// OpWatermark adds a watermark.
	OpWatermark
	// OpRotate rotates an image.
	OpRotate
	// OpBlur applies a Gaussian blur. Useful for thumbnail backgrounds.
	OpBlur
	// OpSharpen sharpens an image. Typically paired with OpResize to recover
	// detail lost during downscaling.
	OpSharpen
	// OpProgressive produces a progressive JPEG. Browsers render progressively,
	// improving perceived load time.
	OpProgressive
	// OpAutoOrient applies the EXIF orientation tag. Mobile cameras often store
	// rotated images with an EXIF tag; without this, thumbnails appear sideways.
	OpAutoOrient
	// OpStripMetadata removes EXIF/IPTC/XMP metadata. Reduces size and strips
	// potentially sensitive info (GPS, camera serial).
	OpStripMetadata
)
