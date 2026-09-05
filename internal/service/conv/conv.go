// Package conv holds pure conversion helpers shared across service subpackages.
// All functions are stateless and depend only on the proto types — safe to call
// from any service subpackage without creating import cycles.
package conv

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/servekit/go-common/jsonx"
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/xcodes"
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

// UploadSandboxPrefix returns the per-owner staging prefix under which
// two-phase upload temp keys are minted: {prefix}tmp/{ownerType}-{ownerID}/.
//
// The owner segment scopes STS policies (a leaked credential can only trash
// its own owner's sandbox, not another owner's in-flight uploads) and keeps
// the tmp namespace debuggable. The random per-session segment added by
// NewTempObjectKey is what makes keys unguessable — this prefix alone must
// never be treated as a secret.
func UploadSandboxPrefix(prefix string, ownerType int32, ownerID int64) string {
	return prefix + "tmp/" + strconv.FormatInt(int64(ownerType), 10) + "-" +
		strconv.FormatInt(ownerID, 10) + "/"
}

// NewTempObjectKey returns a staging key for a two-phase upload:
// {prefix}tmp/{ownerType}-{ownerID}/{128-bit random hex}/{md5}.
//
// SECURITY: the content-addressed final key (ObjectKeyFromMD5) is derivable
// from the MD5 alone, so handing a client a credential for it lets any logged-in
// user overwrite an already-confirmed object with the same hash. Uploads
// therefore write to this unguessable temp key; ConfirmUpload verifies the
// bytes and performs the server-side copy to the final key. The random segment
// is 128 bits — a credential minted for one session is useless against any
// other session, even the same owner's.
//
// Falls back to a fixed (still owner-scoped) key when the system CSPRNG fails,
// returning the error so callers can refuse the upload rather than silently
// issuing a predictable key.
func NewTempObjectKey(prefix string, ownerType int32, ownerID int64, md5 string) (string, error) {
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("generate upload staging key: %w", err)
	}
	return UploadSandboxPrefix(prefix, ownerType, ownerID) + hex.EncodeToString(rnd[:]) + "/" + md5, nil
}

// IsSandboxObjectKey reports whether key was minted by NewTempObjectKey for
// the given owner — i.e. it lives under that owner's staging sandbox prefix.
// GC uses this to decide whether a session's cloud object is staging
// (exclusively owned by the session, safe to delete) or content-addressed
// (globally deduped — must never be deleted from a session's cleanup path).
// Content-address keys cannot collide with the sandbox prefix: their segment
// after {prefix} is the MD5's first two hex chars, never "tmp".
func IsSandboxObjectKey(key, prefix string, ownerType int32, ownerID int64) bool {
	return strings.HasPrefix(key, UploadSandboxPrefix(prefix, ownerType, ownerID))
}

// ResolveBucket returns the provided bucket name if non-empty, otherwise falls
// back to the configured default bucket.
func ResolveBucket(bucket, defaultBucket string) string {
	if bucket != "" {
		return bucket
	}
	return defaultBucket
}

// ResolveBucketForVisibility extends ResolveBucket with the audience-class
// mapping: visibility=PUBLIC uploads always land in the configured public
// bucket (the caller's `bucket` field is ignored — public placement must not
// depend on caller-supplied names), while PRIVATE / UNSPECIFIED keep the
// legacy resolution. Returns an error when PUBLIC is requested but no public
// bucket is configured.
func ResolveBucketForVisibility(bucket, defaultBucket, publicBucket string, visibility storagev1.Visibility) (string, error) {
	if visibility == storagev1.Visibility_VISIBILITY_PUBLIC {
		if publicBucket == "" {
			return "", xcodes.ErrBadRequest.New("visibility=PUBLIC requested but no public bucket is configured")
		}
		return publicBucket, nil
	}
	return ResolveBucket(bucket, defaultBucket), nil
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
