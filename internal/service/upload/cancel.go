package upload

import (
	"context"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// CancelUpload cancels a pending upload session. Best-effort: only succeeds
// when the session is still PENDING. The OSS object is intentionally NOT
// deleted — the client may still be uploading and a delete would race with
// the in-flight PUT. GC reclaims orphaned objects later.
func (s *Service) CancelUpload(ctx context.Context, req *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	token, err := verifyUploadToken(req.GetUploadToken(), s.cfg.Storage.UploadTokenSecret, ownerID, ownerType)
	if err != nil {
		if isUploadTokenExpired(err) {
			return nil, xcodes.ErrUploadTokenExpired.Wrap(err)
		}
		return nil, xcodes.ErrUploadTokenInvalid.Wrap(err)
	}
	if token.SessionID == 0 {
		return nil, xcodes.ErrUploadTokenInvalid.New("legacy token without session_id")
	}

	session, err := dal.GetUploadSessionByID(ctx, s.db, token.SessionID)
	if err != nil {
		return nil, err
	}
	// Cancel only verifies the caller owns the session — MD5/size checks are
	// not relevant because no file record is being created here. OwnerType is
	// checked by verifyUploadToken above; OwnerID is checked here against the
	// persisted session row.
	if session.OwnerID != token.OwnerID || session.OwnerType != token.OwnerType {
		return nil, xcodes.ErrUploadTokenInvalid.New("session/token owner mismatch")
	}

	auditBase := AuditEvent{
		Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CANCEL,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   session.ID,
	}

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return dal.MarkUploadSessionCancelled(ctx, tx, session.ID)
	})
	if txErr != nil {
		s.host.RecordOutcome(ctx, auditBase, txErr)
		return nil, txErr
	}

	s.host.RecordOutcome(ctx, auditBase, nil)
	return &emptypb.Empty{}, nil
}
