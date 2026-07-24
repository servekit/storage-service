package upload

import (
	"context"
	"errors"
	"fmt"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/gorx"
	"github.com/servekit/go-common/xerr"
)

// BatchGetSTSCredential issues shared STS credentials plus per-file upload
// tokens (or instant file IDs on MD5 dedup hit) for a batch of files. The
// shared STS credential is fetched once via sts and reused by every file.
// Per-file processing runs concurrently with bounded parallelism; item order
// matches request order regardless of completion order.
func (s *Service) BatchGetSTSCredential(ctx context.Context, req *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()
	bucket := conv.ResolveBucket(req.GetBucket(), s.cfg.Storage.DefaultBucket)
	ttl := req.GetTtl().AsDuration()

	if n := len(req.GetFiles()); n == 0 {
		return nil, xcodes.ErrBadRequest.New("files is empty")
	} else if max := s.cfg.Storage.Batch.MaxSize; max > 0 && n > max {
		return nil, xcodes.ErrFileBatchTooLarge.New(fmt.Sprintf("max %d files per batch, got %d", max, n))
	}

	// Rate limit: count each file to prevent bypass.
	for range req.GetFiles() {
		if err := s.checkUploadRateLimit(ctx, ownerType, ownerID); err != nil {
			return nil, err
		}
	}

	// Normalize batch-level allowed_extensions once; shared by all files in the
	// batch (batch STS credentials apply one policy to every file).
	allowedExt := normalizeExtensions(req.GetAllowedExtensions())

	// Process files concurrently with bounded parallelism. TaskRunner both
	// throttles (semaphore) and waits; RunSafe adds panic recovery so one
	// panicking file does not abort the whole batch.
	concurrency := s.cfg.Storage.Batch.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}
	items := make([]*storagev1.UploadCredentialItem, len(req.GetFiles()))
	runner := gorx.NewTaskRunner(concurrency)
	group := gorx.NewRoutineGroup()

	for i, f := range req.GetFiles() {
		i, f := i, f
		group.RunSafe(func() {
			runner.Schedule(func() {
				items[i] = s.processOneUpload(ctx, ownerType, ownerID, bucket, ttl, f, req.GetRequestId(), allowedExt)
			})
		})
	}
	group.Wait()
	runner.Wait()

	// Shared STS credential (cached per owner+vendor+bucket; one issuer call max).
	// Per-file issueUploadCredential also calls sts.Get which hits the cache
	// (no extra issuer calls). Total issuer calls for the batch = 1.
	//
	// The shared credential MUST carry the same policy fields the per-file
	// calls use, otherwise (given the STS cache is keyed only on
	// owner+vendor+bucket) the first issuer wins and per-file calls would
	// receive a credential minted under a different (broader) policy.
	vendor := int32(s.registry.VendorForBucket(bucket))
	bucketCfg, err := s.registry.BucketConfig(bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	creds, err := s.sts.Get(ctx, ownerType, ownerID, vendor, bucket, ttl, &storage.STSPolicy{
		OwnerID:           ownerID,
		OwnerType:         ownerType,
		Bucket:            bucket,
		KeyPrefix:         bucketCfg.KeyPrefix,
		AllowedExtensions: allowedExt,
		AllowedActions:    []string{types.PutObjectActionForVendor(vendor)},
		TTL:               ttl,
	})
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "get shared STS")
	}

	return &storagev1.BatchGetSTSCredentialResponse{
		AccessKey:     creds.AccessKey,
		SecretKey:     creds.SecretKey,
		SecurityToken: creds.SecurityToken,
		Endpoint:      creds.Endpoint,
		Bucket:        creds.Bucket,
		ExpiresAt:     creds.ExpiresAt.Unix(),
		Items:         items,
	}, nil
}

// --- internal helpers ---

// processOneUpload runs the per-file flow and maps the result/error into an
// UploadCredentialItem oneof. Errors are reported per-item (not propagated)
// so a single bad file does not fail the whole batch.
func (s *Service) processOneUpload(ctx context.Context, ownerType int32, ownerID int64, bucket string, ttl time.Duration, f *storagev1.UploadFileMeta, requestID string, allowedExtensions []string) *storagev1.UploadCredentialItem {
	// Per-file fail-fast: reject disallowed extensions before any STS call.
	// Mirrors the single-path check in GetSTSCredential; reported as an
	// ItemError so the rest of the batch can still succeed.
	if err := validateFilenameExtension(f.GetFilename(), allowedExtensions); err != nil {
		code := ""
		var xe *xerr.Error
		if errors.As(err, &xe) {
			code = xe.Code().Reason()
		}
		return &storagev1.UploadCredentialItem{
			Result: &storagev1.UploadCredentialItem_Error{
				Error: &storagev1.ItemError{Code: code, Message: err.Error()},
			},
		}
	}

	file := fileMeta{
		md5:         f.GetMd5(),
		size:        f.GetSize(),
		contentType: f.GetContentType(),
		filename:    f.GetFilename(),
		filePath:    f.GetFilePath(),
		description: f.GetDescription(),
		metadata:    f.GetMetadata(),
		requestID:   requestID,
	}
	result, err := s.issueUploadCredential(ctx, ownerType, ownerID, bucket, ttl, file, allowedExtensions)
	if err != nil {
		// issueUploadCredential wraps failures in xerr (e.g. ErrQuotaExceeded).
		// Surface the stable Reason as ItemError.Code so callers can branch on it
		// without parsing the human-readable Message.
		code := ""
		var xe *xerr.Error
		if errors.As(err, &xe) {
			code = xe.Code().Reason()
		}
		return &storagev1.UploadCredentialItem{
			Result: &storagev1.UploadCredentialItem_Error{
				Error: &storagev1.ItemError{Code: code, Message: err.Error()},
			},
		}
	}
	if result.instant {
		return &storagev1.UploadCredentialItem{
			Result: &storagev1.UploadCredentialItem_Token{
				Token: &storagev1.UploadTokenInfo{FileId: result.fileID},
			},
		}
	}
	return &storagev1.UploadCredentialItem{
		Result: &storagev1.UploadCredentialItem_Token{
			Token: &storagev1.UploadTokenInfo{
				UploadToken: result.uploadToken,
				ExpiresAt:   result.expiresAt,
				ObjectKey:   result.objectKey,
			},
		},
	}
}
