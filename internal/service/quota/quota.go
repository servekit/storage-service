// Package quota implements the quota-tracking domain: helper functions for
// checking/reserving/releasing quota, the lower-level get/set/add quota helpers
// (exported so the file/admin subpackages can drive quota via Deps injection),
// and the three quota RPCs (GetMyQuota, SetOwnerQuota, AddOwnerQuota).
package quota

import (
	"context"
	"errors"
	"fmt"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/internal/thirdcall/gid_service"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// Service holds quota-domain dependencies.
type Service struct {
	db                *gorm.DB
	gid               gid_service.GIDService
	audit             audit.Recorder
	defaultQuotaBytes int64
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB                *gorm.DB
	GID               gid_service.GIDService
	Audit             audit.Recorder
	DefaultQuotaBytes int64
}

// New constructs a quota.Service.
func New(d *Deps) *Service {
	return &Service{
		db:                d.DB,
		gid:               d.GID,
		audit:             d.Audit,
		defaultQuotaBytes: d.DefaultQuotaBytes,
	}
}

// CheckQuota verifies that the owner has enough remaining quota for the given bytes.
func (s *Service) CheckQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, requiredBytes int64) error {
	quota, err := s.ensureQuota(ctx, ownerType, ownerID)
	if err != nil {
		return err
	}

	available := availableBytes(quota.TotalBytes, quota.UsedBytes)
	if available < requiredBytes {
		return xcodes.ErrQuotaExceeded.New(fmt.Sprintf("need %d bytes, have %d available", requiredBytes, available))
	}
	return nil
}

// Reserve increases the owner's used quota by the given bytes.
// Caller must wrap in a transaction to guarantee atomicity with other operations.
func (s *Service) Reserve(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, bytes int64) error {
	if _, err := s.ensureQuota(ctx, ownerType, ownerID); err != nil {
		return err
	}
	if err := dal.IncrementQuotaUsed(ctx, db, ownerType, ownerID, bytes); err != nil {
		return xcodes.ErrQuotaExceeded.Wrap(err)
	}
	return nil
}

// Release decreases the owner's used quota by the given bytes.
// Caller must wrap in a transaction to guarantee atomicity with other operations.
func (s *Service) Release(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, bytes int64) error {
	return dal.DecrementQuotaUsed(ctx, db, ownerType, ownerID, bytes)
}

// GetQuota returns the owner's quota info.
func (s *Service) GetQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID int64) (*models.StorageQuota, error) {
	return s.ensureQuota(ctx, ownerType, ownerID)
}

// SetQuota updates the total quota for an owner.
// Caller must wrap in a transaction to guarantee atomicity with other operations.
func (s *Service) SetQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, totalBytes int64) error {
	if _, err := s.ensureQuota(ctx, ownerType, ownerID); err != nil {
		return err
	}
	return dal.SetQuota(ctx, db, ownerType, ownerID, totalBytes)
}

// AddQuota atomically increments the owner's total quota by delta (may be negative).
// Caller must wrap in a transaction to guarantee atomicity with other operations.
func (s *Service) AddQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, delta int64) error {
	if _, err := s.ensureQuota(ctx, ownerType, ownerID); err != nil {
		return err
	}
	return dal.AddQuota(ctx, db, ownerType, ownerID, delta)
}

// GetMyQuota returns the caller's current storage quota and usage. The quota
// row is created on first reference if it does not yet exist.
func (s *Service) GetMyQuota(ctx context.Context, req *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	q, err := s.GetQuota(ctx, s.db, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrQuotaNotFound.Wrap(err)
	}

	fileCount, err := dal.CountFilesByOwner(ctx, s.db, ownerID, ownerType)
	if err != nil {
		return nil, fmt.Errorf("count user files: %w", err)
	}

	available := availableBytes(q.TotalBytes, q.UsedBytes)

	return &storagev1.QuotaInfo{
		TotalBytes:     q.TotalBytes,
		UsedBytes:      q.UsedBytes,
		AvailableBytes: available,
		FileCount:      int32(fileCount),
	}, nil
}

// SetOwnerQuota is the business-facing RPC to overwrite an owner's total quota.
// Use this when a business caller (pay-service, plan-change flow) wants to
// reset a user's quota to a known value. For admin tooling use AdminSetQuota.
//
// TODO(auth): add service-to-service authentication (shared secret / mTLS) before
// exposing this in production. See go-common/grpcx/auth.go for helpers.
func (s *Service) SetOwnerQuota(ctx context.Context, req *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	oldQuota, oldQuotaErr := s.GetQuota(ctx, s.db, ownerType, ownerID)
	var oldTotalBytes int64
	if oldQuotaErr == nil {
		oldTotalBytes = oldQuota.TotalBytes
	}

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_SET_OWNER_QUOTA,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_QUOTA,
		TargetID:   ownerID,
		Before:     conv.MustToMap(audit.QuotaSnapshot{TotalBytes: oldTotalBytes}),
		After:      conv.MustToMap(audit.QuotaSnapshot{TotalBytes: req.GetTotalBytes()}),
	}

	var result *storagev1.QuotaInfo
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.SetQuota(ctx, tx, ownerType, ownerID, req.GetTotalBytes()); err != nil {
			return err
		}
		result = s.buildQuotaInfo(ctx, tx, ownerType, ownerID)
		return nil
	})
	if txErr != nil {
		s.audit.RecordOutcome(ctx, auditBase, txErr)
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}

	s.audit.RecordOutcome(ctx, auditBase, nil)
	return result, nil
}

// AddOwnerQuota is the business-facing RPC to atomically increment quota.
// delta_bytes is positive for purchase, negative for refund. The caller is
// responsible for idempotency (e.g. deduplicate by order ID upstream).
//
// TODO(auth): same as SetOwnerQuota.
func (s *Service) AddOwnerQuota(ctx context.Context, req *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	oldQuota, oldQuotaErr := s.GetQuota(ctx, s.db, ownerType, ownerID)
	var oldTotalBytes int64
	if oldQuotaErr == nil {
		oldTotalBytes = oldQuota.TotalBytes
	}

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_ADD_OWNER_QUOTA,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_QUOTA,
		TargetID:   ownerID,
		Before:     conv.MustToMap(audit.QuotaSnapshot{TotalBytes: oldTotalBytes}),
		After:      conv.MustToMap(audit.QuotaSnapshot{TotalBytes: oldTotalBytes + req.GetDeltaBytes()}),
	}

	var result *storagev1.QuotaInfo
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.AddQuota(ctx, tx, ownerType, ownerID, req.GetDeltaBytes()); err != nil {
			return err
		}
		result = s.buildQuotaInfo(ctx, tx, ownerType, ownerID)
		return nil
	})
	if txErr != nil {
		s.audit.RecordOutcome(ctx, auditBase, txErr)
		// Distinct error: refund-too-large surfaces as ErrQuotaInsufficientTotal;
		// other failures stay ErrInternal.
		if errors.Is(txErr, xcodes.ErrQuotaInsufficientTotal.New()) {
			return nil, xcodes.ErrQuotaInsufficientTotal.Wrap(txErr)
		}
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}

	s.audit.RecordOutcome(ctx, auditBase, nil)
	return result, nil
}

// --- internal helpers ---

// availableBytes returns the non-negative remaining quota (total - used),
// clamped to zero when used exceeds total. This is the pure-Go core of the
// "do we have room?" calculation shared by CheckQuota and GetMyQuota; keeping
// it extracted makes the overflow/clamp edge cases unit-testable without a DB.
//
// Note: int64 subtraction never panics on overflow, but a negative result
// means used > total (e.g. quota was reduced below current usage) which must
// surface as 0 available, not a wrapped positive value.
func availableBytes(total, used int64) int64 {
	available := total - used
	if available < 0 {
		return 0
	}
	return available
}

// buildQuotaInfo builds the proto QuotaInfo response from the current quota row
// plus the owner's file count. The db argument may be a transaction; reads happen
// against that connection so the result reflects in-progress changes.
//
// Returns nil if the quota row cannot be loaded — callers should treat that as
// a transaction failure and rollback.
func (s *Service) buildQuotaInfo(ctx context.Context, db *gorm.DB, ownerType int32, ownerID int64) *storagev1.QuotaInfo {
	q, err := s.GetQuota(ctx, db, ownerType, ownerID)
	if err != nil {
		return nil
	}
	fileCount, err := dal.CountFilesByOwner(ctx, db, ownerID, ownerType)
	if err != nil {
		return nil
	}
	return &storagev1.QuotaInfo{
		TotalBytes:     q.TotalBytes,
		UsedBytes:      q.UsedBytes,
		AvailableBytes: q.TotalBytes - q.UsedBytes,
		FileCount:      int32(fileCount),
	}
}

// ensureQuota returns the owner's quota, creating one with default bytes if not exists.
//
// Uses s.db (NOT the caller's transaction) because:
//  1. The operation is idempotent: CreateIfNotExist uses INSERT ... ON CONFLICT
//     DO NOTHING and re-reads the winning row on collision.
//  2. It calls s.gid.NextID, which is a gRPC call to gid-service and can be slow.
//  3. Running gRPC inside the caller's DB transaction would hold the outer tx
//     open (and its row locks) for the duration of the gRPC call, causing lock
//     contention and latency spikes.
//
// Concurrent callers each ensure the quota row; the loser's CreateIfNotExist
// becomes a no-op and re-reads the winner's row. At most one extra snowflake ID
// is wasted per concurrent race — benign and documented.
func (s *Service) ensureQuota(ctx context.Context, ownerType int32, ownerID int64) (*models.StorageQuota, error) {
	quota, err := dal.GetQuotaByOwner(ctx, s.db, ownerType, ownerID)
	if err == nil {
		return quota, nil
	}

	id, gidErr := s.gid.NextID(ctx)
	if gidErr != nil {
		return nil, fmt.Errorf("generate quota id: %w", gidErr)
	}
	quota, err = dal.CreateQuotaIfNotExist(ctx, s.db, id, ownerType, ownerID, s.defaultQuotaBytes)
	if err != nil {
		return nil, fmt.Errorf("ensure quota: %w", err)
	}
	return quota, nil
}
