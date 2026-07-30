package audit

import (
	"context"
	"log/slog"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/thirdcall/gid_service"

	"gorm.io/gorm"
)

// Recorder records audit events. Implementations must be safe for concurrent use.
//
// Two recording modes are provided:
//
//   - Record(ctx, event): writes via the recorder's own DB handle, OUTSIDE any
//     caller transaction. Failure is logged but does not block or roll back the
//     business operation. This is the default mode for most call sites.
//
//   - RecordInTx(ctx, tx, event): writes via the caller's *gorm.DB transaction,
//     so the audit row commits or rolls back together with the business write.
//     Use this only for operations where audit completeness is a hard requirement
//     (e.g. AdminSetQuota, AdminDelete) — the audit must not survive a rolled-
//     back business tx, and a missing audit row must not survive a committed one.
//
// Design trade-off (I6): the default Record mode intentionally decouples audit
// durability from business tx outcomes. The cost: if the process crashes between
// the caller's tx commit and Record completing, that audit row is lost. We
// accept this trade-off because storage-service's primary contract is data
// integrity of files/objects; audit is a forensic aid, not a transactional
// precondition. Forcing audit into every business tx would make every audit
// write a potential SPOF (audit DB hiccup → uploads fail). RecordInTx exists
// for the small set of operations where that trade-off goes the other way.
//
// Both modes swallow non-fatal errors internally (logged, not propagated):
// audit failure must not block business operations.
type Recorder interface {
	Record(ctx context.Context, event Event) error
	RecordInTx(ctx context.Context, tx *gorm.DB, event Event) error
	// RecordOutcome records an audit event with Status derived from err:
	// err != nil → FAILED, err == nil → SUCCESS. base carries the per-operation
	// fields (Action, Owner*, Target*, Before, After, RequestID). Errors from
	// Record itself are logged and not surfaced (audit must not block business
	// ops — see Recorder docs for the I6 trade-off).
	RecordOutcome(ctx context.Context, base Event, err error)
	// RecordOutcomeInTx is the tx-bound variant of RecordOutcome. tx must be the
	// same *gorm.DB the caller's business write is using.
	RecordOutcomeInTx(ctx context.Context, tx *gorm.DB, base Event, err error)
}

// DBRecorder writes audit events to the database.
type DBRecorder struct {
	db  *gorm.DB
	gid gid_service.GIDService
}

// NewDBRecorder creates a new DBRecorder.
func NewDBRecorder(db *gorm.DB, gid gid_service.GIDService) *DBRecorder {
	return &DBRecorder{db: db, gid: gid}
}

// Record persists an audit event to the database.
//
// This write happens outside any caller transaction (see Recorder docs for the
// I6 trade-off). Callers should invoke Record AFTER their business tx commits
// — emitting it before commit means a subsequent tx rollback would leave a
// misleading audit row claiming success.
//
// Errors are logged but not propagated — audit failure must not block business operations.
func (r *DBRecorder) Record(ctx context.Context, event Event) error {
	id, err := r.gid.NextID(ctx)
	if err != nil {
		slog.Error("audit: generate id", "error", err)
		return err
	}
	if createErr := dal.CreateAuditLog(ctx, r.db, buildAuditLog(id, event)); createErr != nil {
		slog.Error("audit: write log", "error", createErr)
		return createErr
	}
	return nil
}

// RecordInTx persists an audit event using the caller's transaction.
//
// The audit row commits or rolls back together with the business write. Call
// this from INSIDE the business tx, after the business operation succeeded.
//
// ID generation happens BEFORE the tx-scoped write so we don't hold the tx
// open across the gid gRPC call (same pattern as ensureQuota).
//
// Errors are logged but not propagated — callers in the failure path still
// need their original error, not the audit write error.
func (r *DBRecorder) RecordInTx(ctx context.Context, tx *gorm.DB, event Event) error {
	id, err := r.gid.NextID(ctx)
	if err != nil {
		slog.Error("audit: generate id (in tx)", "error", err)
		return err
	}
	if createErr := dal.CreateAuditLog(ctx, tx, buildAuditLog(id, event)); createErr != nil {
		slog.Error("audit: write log (in tx)", "error", createErr)
		return createErr
	}
	return nil
}

// RecordOutcome records an audit event with Status derived from err:
//   - err != nil → AUDIT_LOG_STATUS_FAILED, err attached
//   - err == nil → AUDIT_LOG_STATUS_SUCCESS
//
// base carries the per-operation fields (Action, Owner*, Target*, Before,
// After, RequestID). Callers build it once per request and call this helper
// from the success path and the failure path, instead of duplicating the
// Event literal in both branches. Status/Error are the only fields that differ
// between the two recordings.
//
// Errors from Record itself are logged by DBRecorder and not surfaced here —
// see Recorder docs for the I6 trade-off (audit must not block business ops).
func (r *DBRecorder) RecordOutcome(ctx context.Context, base Event, err error) {
	ev := base
	if err != nil {
		ev.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED
		ev.Error = err
	} else {
		ev.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS
	}
	// DBRecorder.Record already logs internally; surface residual errors at
	// warn so a broken audit pipeline (e.g. DB disconnect) doesn't disappear
	// silently. We must not propagate: this runs on the success path too.
	if recErr := r.Record(ctx, ev); recErr != nil {
		slog.Warn("audit: record outcome", "error", recErr)
	}
}

// RecordOutcomeInTx is the tx-bound variant of RecordOutcome. Use this for
// operations where the audit row must commit/rollback with the business tx
// (e.g. admin quota writes). tx must be the same *gorm.DB the caller's business
// write is using. Caller is responsible for invoking this BEFORE returning from
// the tx func so the audit write participates in the commit decision.
func (r *DBRecorder) RecordOutcomeInTx(ctx context.Context, tx *gorm.DB, base Event, err error) {
	ev := base
	if err != nil {
		ev.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED
		ev.Error = err
	} else {
		ev.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS
	}
	if recErr := r.RecordInTx(ctx, tx, ev); recErr != nil {
		slog.Warn("audit: record outcome (in tx)", "error", recErr)
	}
}
