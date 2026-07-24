package s3

import "github.com/servekit/storage-service/internal/provider/storage/types"

// buildS3ProcessStyle translates typed ops to an S3-compatible image
// processing directive. S3 has no native equivalent of Aliyun's
// x-oss-process — image transforms typically run via Lambda Function URLs,
// CloudFront image resizing, or a separate image pipeline.
//
// This stub mirrors aliyun.buildOssProcessStyle so the PresignGetObject
// plumbing is symmetric across vendors. Currently always returns empty
// string; PresignGetObject interprets empty as "unsupported" and surfaces
// types.ErrImageProcessingUnsupported so callers fail loudly rather than
// getting a silently-unprocessed image.
//
// Future integration: implement this to return a non-empty directive (e.g.,
// a Lambda Function URL query string), then add the corresponding request
// field wiring in PresignGetObject.
func buildS3ProcessStyle(_ []types.Op) string {
	return ""
}
