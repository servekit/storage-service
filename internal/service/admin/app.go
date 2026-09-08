// App admin RPCs: calling-application CRUD for the storage platform.
// Mirrors message-service's app management — app_key/key_prefix are
// immutable, the secret is minted server-side and shown exactly once
// (create / rotate), deletion is soft and takes effect on the next registry
// refresh (which runs immediately after each mutation below).
package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
)

// AdminCreateApp registers a calling application. app_key empty = minted
// server-side. The secret is returned once and never listed.
func (s *Service) AdminCreateApp(ctx context.Context, req *storagev1.AdminCreateAppRequest) (*storagev1.AdminCreateAppResponse, error) {
	if req.GetKeyPrefix() == "" {
		return nil, xcodes.ErrBadRequest.New("key_prefix is required")
	}
	appKey := req.GetAppKey()
	if appKey == "" {
		appKey = mintAppKey()
	}
	if _, err := dal.GetAppByKey(ctx, s.db, appKey); err == nil {
		return nil, xcodes.ErrAppExists.New(fmt.Sprintf("app_key %q already exists", appKey))
	} else if !errors.Is(err, xcodes.ErrAppNotFound.New()) {
		return nil, err
	}
	if n, err := dal.CountAppsByKeyPrefix(ctx, s.db, req.GetKeyPrefix()); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, xcodes.ErrPrefixTaken.New(fmt.Sprintf("key_prefix %q already in use", req.GetKeyPrefix()))
	}
	if req.GetBucketId() != 0 {
		if _, err := dal.GetBucketByID(ctx, s.db, req.GetBucketId()); err != nil {
			return nil, err
		}
	}

	secret, err := mintAppSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "generate app id")
	}
	app := &models.StorageApp{
		ID:        id,
		AppKey:    appKey,
		AppSecret: secret,
		Name:      req.GetName(),
		KeyPrefix: req.GetKeyPrefix(),
		BucketID:  req.GetBucketId(),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_CREATE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, nil, appSnapshot(app))
	s.refreshPlatform(ctx)
	return &storagev1.AdminCreateAppResponse{App: appToProto(app), AppSecret: secret}, nil
}

// AdminGetApp returns one app by app_key.
func (s *Service) AdminGetApp(ctx context.Context, req *storagev1.AdminGetAppRequest) (*storagev1.AdminGetAppResponse, error) {
	app, err := dal.GetAppByKey(ctx, s.db, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	return &storagev1.AdminGetAppResponse{App: appToProto(app)}, nil
}

// AdminUpdateApp edits mutable fields. app_key and key_prefix are immutable
// (objects already live under the prefix); bucket rebinding only affects
// new uploads.
func (s *Service) AdminUpdateApp(ctx context.Context, req *storagev1.AdminUpdateAppRequest) (*storagev1.AdminUpdateAppResponse, error) {
	app, err := dal.GetAppByKey(ctx, s.db, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	before := appSnapshot(app)
	if req.Name != nil && *req.Name != "" {
		app.Name = *req.Name
	}
	if req.Disabled != nil {
		app.Disabled = *req.Disabled
	}
	if req.BucketId != nil {
		if *req.BucketId != 0 {
			if _, err := dal.GetBucketByID(ctx, s.db, *req.BucketId); err != nil {
				return nil, err
			}
		}
		app.BucketID = *req.BucketId
	}
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPDATE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, before, appSnapshot(app))
	s.refreshPlatform(ctx)
	return &storagev1.AdminUpdateAppResponse{App: appToProto(app)}, nil
}

// AdminRotateAppSecret mints a new secret; the old one stops working on the
// next registry refresh (immediate here).
func (s *Service) AdminRotateAppSecret(ctx context.Context, req *storagev1.AdminRotateAppSecretRequest) (*storagev1.AdminRotateAppSecretResponse, error) {
	app, err := dal.GetAppByKey(ctx, s.db, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	secret, err := mintAppSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if err := dal.UpdateAppSecret(ctx, s.db, app.ID, secret); err != nil {
		return nil, err
	}
	s.refreshPlatform(ctx)
	return &storagev1.AdminRotateAppSecretResponse{App: appToProto(app), AppSecret: secret}, nil
}

// AdminListApps lists all live apps (low cardinality, no paging).
func (s *Service) AdminListApps(ctx context.Context, _ *storagev1.AdminListAppsRequest) (*storagev1.AdminListAppsResponse, error) {
	apps, err := dal.ListApps(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]*storagev1.StorageAppInfo, 0, len(apps))
	for _, a := range apps {
		out = append(out, appToProto(a))
	}
	return &storagev1.AdminListAppsResponse{Apps: out}, nil
}

// AdminDeleteApp soft-deletes the app. Data-plane calls fail on the next
// registry refresh (immediate here); existing objects/files stay readable —
// their keys are stored on the rows.
func (s *Service) AdminDeleteApp(ctx context.Context, req *storagev1.AdminDeleteAppRequest) (*emptypb.Empty, error) {
	app, err := dal.GetAppByKey(ctx, s.db, req.GetAppKey())
	if err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, app.ID); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, appSnapshot(app), nil)
	s.refreshPlatform(ctx)
	return &emptypb.Empty{}, nil
}

// --- helpers ---

func appToProto(a *models.StorageApp) *storagev1.StorageAppInfo {
	return &storagev1.StorageAppInfo{
		Id:        a.ID,
		AppKey:    a.AppKey,
		Name:      a.Name,
		KeyPrefix: a.KeyPrefix,
		BucketId:  a.BucketID,
		Disabled:  a.Disabled,
		CreatedAt: a.CreatedAt.Unix(),
		UpdatedAt: a.UpdatedAt.Unix(),
	}
}

func appSnapshot(a *models.StorageApp) map[string]any {
	return map[string]any{
		"app_key": a.AppKey, "name": a.Name, "key_prefix": a.KeyPrefix,
		"bucket_id": a.BucketID, "disabled": a.Disabled,
	}
}

// mintAppKey mints "sto_" + 8 base36 chars (same shape as message-service).
func mintAppKey() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "sto_" + fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = base36[int(b)%36]
	}
	return "sto_" + string(out)
}

// mintAppSecret mints "sto_" + 32 random bytes (base64url).
func mintAppSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint app secret: %w", err)
	}
	return "sto_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
