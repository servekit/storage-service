// Package conv holds pure conversion helpers shared across service subpackages.
// All functions are stateless and depend only on the proto types — safe to call
// from any service subpackage without creating import cycles.
package conv

import (
	"fmt"
	"log/slog"

	"github.com/servekit/go-common/jsonx"
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// OwnerTypeToProto converts an int32 owner_type DB value to its proto enum.
func OwnerTypeToProto(t int32) storagev1.OwnerType {
	return storagev1.OwnerType(t)
}

// VendorToName maps a proto Vendor int32 to its enum name string (e.g. 2 →
// "VENDOR_AWS_S3"). Returns "" for VENDOR_UNSPECIFIED or unknown values.
func VendorToName(v int32) string {
	if storagev1.Vendor(v) == storagev1.Vendor_VENDOR_UNSPECIFIED {
		return ""
	}
	name, ok := storagev1.Vendor_name[v]
	if !ok {
		return ""
	}
	return name
}

// ACLToProto converts a string ACL key to its proto enum.
func ACLToProto(acl string) storagev1.BucketACL {
	switch acl {
	case "private":
		return storagev1.BucketACL_BUCKET_ACL_PRIVATE
	case "public_read":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ
	case "public_read_write":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE
	default:
		return storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED
	}
}

// ObjectKeyFromMD5 builds the storage object key from a prefix and MD5 hash.
// Format: {prefix}{md5[:2]}/{md5}
func ObjectKeyFromMD5(prefix, md5 string) string {
	if len(md5) < 2 {
		return prefix + md5
	}
	return prefix + md5[:2] + "/" + md5
}

// ResolveBucket returns the provided bucket name if non-empty, otherwise falls
// back to the configured default bucket.
func ResolveBucket(bucket, defaultBucket string) string {
	if bucket != "" {
		return bucket
	}
	return defaultBucket
}

// ProtoToImageOp converts a proto ImageProcessOp to a types.Op. Callers pass
// the resulting []Op to Provider.PresignGetObject via types.WithImageOps.
func ProtoToImageOp(op *storagev1.ImageProcessOp) types.Op {
	if op == nil {
		return types.Op{}
	}

	var opType types.OpType
	switch op.GetType() {
	case storagev1.ImageProcessOp_TYPE_RESIZE:
		opType = types.OpResize
	case storagev1.ImageProcessOp_TYPE_CROP:
		opType = types.OpCrop
	case storagev1.ImageProcessOp_TYPE_QUALITY:
		opType = types.OpQuality
	case storagev1.ImageProcessOp_TYPE_FORMAT:
		opType = types.OpFormat
	case storagev1.ImageProcessOp_TYPE_WATERMARK:
		opType = types.OpWatermark
	case storagev1.ImageProcessOp_TYPE_ROTATE:
		opType = types.OpRotate
	default:
		opType = types.OpResize
	}

	return types.Op{
		Type:          opType,
		Width:         int(op.GetWidth()),
		Height:        int(op.GetHeight()),
		Format:        op.GetFormat(),
		Quality:       int(op.GetQuality()),
		ResizeMode:    op.GetResizeMode(),
		WatermarkText: op.GetWatermarkText(),
		RotateDegrees: int(op.GetRotateDegrees()),
	}
}

// MustToMap converts a struct to a map[string]any via JSON round-trip. Field
// names come from json tags. Returns nil on failure (already logged via slog);
// business logic is not blocked. Used by audit recording to capture snapshot
// state.
func MustToMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	b, err := jsonx.Marshal(v)
	if err != nil {
		slog.Error("conv: marshal snapshot", "type", fmt.Sprintf("%T", v), "error", err)
		return nil
	}
	var m map[string]any
	if err := jsonx.Unmarshal(b, &m); err != nil {
		slog.Error("conv: unmarshal snapshot", "type", fmt.Sprintf("%T", v), "error", err)
		return nil
	}
	return m
}
