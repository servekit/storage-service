// Admin file-surface scope matrix (tenant platform phase ④ Q11): files DO
// carry a tenant dimension — every row belongs to exactly one tenant via
// tenant_key (phase ③ write-path), so unlike the dimension-less Admin*
// surfaces the file list/get/delete answer a tenant-scoped caller too:
//
//	injected key   → own-tenant rows only (list filters at the DB; get /
//	                 delete of a foreign row answers ErrFileNotFound —
//	                 anti-enumeration, the app-surface rule)
//	PLATFORM actor → cross-view: every row incl. the unattributed ones,
//	                 plus the tenant_key list filter
//	no identity    → fail closed (covered by TestAdminScope_NoIdentityFailsClosed)
//
// NULL-row rule (documented decision): a pre-③ row the ③ T6 backfill could
// not attribute (app_key=” history) belongs to no tenant, so it is visible
// to the cross-view ONLY — a scoped caller never sees it, in lists or by id.
package admin

import (
	"context"
	"testing"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedScopedFiles plants one file per attribution state: alpha, beta and an
// unattributable (NULL) row, each on its own object so get/delete paths work.
// The owner's quota row is pre-charged with the files' bytes — a real upload
// would have reserved them, and AdminDeleteFile releases on the way out.
// Returns the three file ids.
func seedScopedFiles(t *testing.T, s *Service) (alphaID, betaID, nullID int64) {
	t.Helper()
	ctx := context.Background()
	quotaRow := &models.StorageQuota{OwnerType: 1, OwnerID: 77, TotalBytes: 1 << 20, UsedBytes: 0}
	for i, tc := range []struct {
		name   string
		tenant *string
	}{
		{"alpha.txt", models.TenantKeyPtr(scopeAlpha)},
		{"beta.txt", models.TenantKeyPtr(scopeBeta)},
		{"legacy.txt", nil},
	} {
		obj := &models.StorageObject{
			ID: int64(9100 + i), Vendor: 1, Bucket: "bkt", ObjectKey: tc.name,
			MD5: tc.name, Size: 100, ContentType: "text/plain", RefCount: 1,
		}
		_, _, err := dal.CreateOrGetObject(ctx, s.db, obj)
		require.NoError(t, err)
		f := &models.StorageFile{
			ID: int64(9200 + i), OwnerType: 1, OwnerID: 77, ObjectID: obj.ID,
			TenantKey: tc.tenant, Filename: tc.name,
		}
		require.NoError(t, dal.CreateFile(ctx, s.db, f))
		quotaRow.UsedBytes += obj.Size
		switch i {
		case 0:
			alphaID = f.ID
		case 1:
			betaID = f.ID
		case 2:
			nullID = f.ID
		}
	}
	require.NoError(t, s.db.WithContext(ctx).Create(quotaRow).Error)
	return alphaID, betaID, nullID
}

// TestAdminFileScope_TenantViewOwnRowsOnly: an injected key lists only the
// tenant's own rows — the foreign tenant's file and the unattributed row are
// both absent, and the rows echo their tenant_key.
func TestAdminFileScope_TenantViewOwnRowsOnly(t *testing.T) {
	svc := newScopeFixture(t)
	alphaID, betaID, nullID := seedScopedFiles(t, svc)

	resp, err := svc.AdminListFiles(tenantCtx(scopeAlpha), &storagev1.AdminListFilesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetFiles(), 1)
	require.EqualValues(t, 1, resp.GetTotalCount())
	got := resp.GetFiles()[0]
	assert.Equal(t, alphaID, got.GetId())
	assert.Equal(t, scopeAlpha, got.GetTenantKey())
	assert.NotEqual(t, betaID, got.GetId())
	assert.NotEqual(t, nullID, got.GetId())

	// A forged tenant_key filter in the body must NOT widen the view: the
	// injection outranks the request (clamp, the app-surface rule).
	resp, err = svc.AdminListFiles(tenantCtx(scopeAlpha), &storagev1.AdminListFilesRequest{TenantKey: scopeBeta})
	require.NoError(t, err)
	require.Len(t, resp.GetFiles(), 1)
	assert.Equal(t, alphaID, resp.GetFiles()[0].GetId())
}

// TestAdminFileScope_NullRowsCrossViewOnly: the unattributable row is listed
// and fetched by the cross-view but hidden from a scoped caller (list absence
// + get by id answers the domain not-found).
func TestAdminFileScope_NullRowsCrossViewOnly(t *testing.T) {
	svc := newScopeFixture(t)
	_, _, nullID := seedScopedFiles(t, svc)

	// scoped caller: hidden in the list…
	resp, err := svc.AdminListFiles(tenantCtx(scopeAlpha), &storagev1.AdminListFilesRequest{})
	require.NoError(t, err)
	for _, f := range resp.GetFiles() {
		assert.NotEqual(t, nullID, f.GetId())
	}
	// …and by id (not-found, not forbidden — anti-enumeration).
	_, err = svc.AdminGetFile(tenantCtx(scopeAlpha), &storagev1.AdminGetFileRequest{FileId: nullID})
	assert.ErrorIs(t, err, xcodes.ErrFileNotFound.New())

	// cross-view: present, with the empty tenant_key echo.
	all, err := svc.AdminListFiles(platformCtx(), &storagev1.AdminListFilesRequest{})
	require.NoError(t, err)
	var sawNull bool
	for _, f := range all.GetFiles() {
		if f.GetId() == nullID {
			sawNull = true
			assert.Empty(t, f.GetTenantKey())
		}
	}
	assert.True(t, sawNull, "cross-view must see the unattributed row")

	got, err := svc.AdminGetFile(platformCtx(), &storagev1.AdminGetFileRequest{FileId: nullID})
	require.NoError(t, err)
	assert.Empty(t, got.GetTenantKey())
}

// TestAdminFileScope_CrossTenantDenial: get/delete of another tenant's file
// answers ErrFileNotFound and leaves the row untouched — a scoped caller
// never learns the file exists.
func TestAdminFileScope_CrossTenantDenial(t *testing.T) {
	svc := newScopeFixture(t)
	_, betaID, _ := seedScopedFiles(t, svc)
	ctx := tenantCtx(scopeAlpha)

	_, err := svc.AdminGetFile(ctx, &storagev1.AdminGetFileRequest{FileId: betaID})
	assert.ErrorIs(t, err, xcodes.ErrFileNotFound.New())

	_, err = svc.AdminDeleteFile(ctx, &storagev1.AdminDeleteFileRequest{FileId: betaID})
	assert.ErrorIs(t, err, xcodes.ErrFileNotFound.New())

	f, err := dal.GetFileByID(context.Background(), svc.db, betaID)
	require.NoError(t, err)
	require.NotNil(t, f, "the foreign row must survive the denied delete")
}

// TestAdminFileScope_ScopedGetDeleteOwn: the happy path for a tenant admin —
// own-tenant get answers full metadata, delete removes the row (and the
// refcount/quota bookkeeping runs exactly as for the platform operator).
func TestAdminFileScope_ScopedGetDeleteOwn(t *testing.T) {
	svc := newScopeFixture(t)
	alphaID, _, _ := seedScopedFiles(t, svc)
	ctx := tenantCtx(scopeAlpha)

	got, err := svc.AdminGetFile(ctx, &storagev1.AdminGetFileRequest{FileId: alphaID})
	require.NoError(t, err)
	assert.Equal(t, "alpha.txt", got.GetFilename())
	assert.Equal(t, scopeAlpha, got.GetTenantKey())

	_, err = svc.AdminDeleteFile(ctx, &storagev1.AdminDeleteFileRequest{FileId: alphaID})
	require.NoError(t, err)

	_, err = dal.GetFileByID(context.Background(), svc.db, alphaID)
	assert.ErrorIs(t, err, xcodes.ErrFileNotFound.New())
}

// TestAdminFileScope_CrossViewTenantFilter: the PLATFORM cross-view keeps
// the whole surface and gains the tenant_key filter (the additive field).
func TestAdminFileScope_CrossViewTenantFilter(t *testing.T) {
	svc := newScopeFixture(t)
	_, betaID, nullID := seedScopedFiles(t, svc)

	resp, err := svc.AdminListFiles(platformCtx(), &storagev1.AdminListFilesRequest{TenantKey: scopeBeta})
	require.NoError(t, err)
	require.Len(t, resp.GetFiles(), 1)
	assert.Equal(t, betaID, resp.GetFiles()[0].GetId())
	assert.Equal(t, scopeBeta, resp.GetFiles()[0].GetTenantKey())

	// empty filter = no tenant constraint (the NULL row included)
	resp, err = svc.AdminListFiles(platformCtx(), &storagev1.AdminListFilesRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.GetFiles(), 3)
	_ = nullID
}
