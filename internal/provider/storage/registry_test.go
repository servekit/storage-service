package storage

import (
	"testing"

	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/pkg/config"
)

// All Phase 1 vendors (Tencent, Huawei, Volcengine) are now implemented and
// have their own dispatch/wiring tests below. The "not yet implemented"
// placeholder tests have been retired.

// TestNewProvider_HuaweiDispatchSucceeds verifies Huawei config now
// dispatches to huawei.NewHuaweiProvider and returns a concrete provider
// (no "not yet implemented" error). role_arn + domain_id intentionally
// omitted — provider must still construct (STS path is opt-in at
// GetSTSToken time).
func TestNewProvider_HuaweiDispatchSucceeds(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "huawei-test",
		Vendor:    "VENDOR_HUAWEI_OBS",
		Endpoint:  "obs.cn-north-4.myhuaweicloud.com",
		Region:    "cn-north-4",
		AccessKey: "ak",
		SecretKey: "sk",
	}
	p, err := newProvider(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)
}

// TestNewCDNURLGenerator_HuaweiDispatchSucceeds verifies Huawei CDN config
// dispatches to huawei.NewCDNURLGenerator.
func TestNewCDNURLGenerator_HuaweiDispatchSucceeds(t *testing.T) {
	cdn := &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: "k",
	}
	gen, err := newCDNURLGenerator("VENDOR_HUAWEI_OBS", cdn)
	require.NoError(t, err)
	require.NotNil(t, gen)
}

// TestNewProvider_TencentDispatch verifies Tencent COS constructs a real
// *tencent.TencentProvider and that the dispatch correctly passes through
// endpoint/credentials. The returned Provider must NOT be a placeholder.
func TestNewProvider_TencentDispatch(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "tencent-test",
		Vendor:    "VENDOR_TENCENT_COS",
		Endpoint:  "http://cos.ap-guangzhou.myqcloud.com",
		Region:    "ap-guangzhou",
		AccessKey: "ak",
		SecretKey: "sk",
		// RoleARN intentionally empty — Tencent CAM STS doesn't use it.
		Buckets: []*config.BucketConfig{
			{Name: "mybucket-1250000000", ACL: "private"},
		},
	}
	p, err := newProvider(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)
	// Provider type is opaque at this layer (it's behind the storage.Provider
	// interface) — verify it doesn't return the "not yet implemented" error
	// path by simply checking the call succeeded. A separate dispatch test
	// that type-asserts *tencent.TencentProvider lives in the tencent package.
}

// TestNewProvider_TencentRejectsRoleARN verifies that a Tencent provider
// config with a non-empty RoleARN fails at registry dispatch.
func TestNewProvider_TencentRejectsRoleARN(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "tencent-test",
		Vendor:    "VENDOR_TENCENT_COS",
		Endpoint:  "http://cos.ap-guangzhou.myqcloud.com",
		Region:    "ap-guangzhou",
		AccessKey: "ak",
		SecretKey: "sk",
		RoleARN:   "this-should-be-empty",
		Buckets: []*config.BucketConfig{
			{Name: "mybucket-1250000000"},
		},
	}
	_, err := newProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role_arn must be empty")
}

// TestNewProvider_TencentRejectsMultiBucket verifies that a Tencent provider
// config declaring more than one bucket fails at registry dispatch.
// cos-go-sdk-v5 binds its client to one bucket URL at construction; allowing
// multiple buckets would silently route every request to whichever bucket is
// embedded in the endpoint URL, so the multi-bucket case must fail loudly.
func TestNewProvider_TencentRejectsMultiBucket(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "tencent-test",
		Vendor:    "VENDOR_TENCENT_COS",
		Endpoint:  "http://cos.ap-guangzhou.myqcloud.com",
		Region:    "ap-guangzhou",
		AccessKey: "ak",
		SecretKey: "sk",
		Buckets: []*config.BucketConfig{
			{Name: "bucket-a-1250000000"},
			{Name: "bucket-b-1250000000"},
		},
	}
	_, err := newProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket-bound")
	assert.Contains(t, err.Error(), "split into one provider per bucket")
}

// TestNewCDNURLGenerator_TencentDispatch verifies Tencent CDN generator
// dispatches to tencent.NewCDNURLGenerator.
func TestNewCDNURLGenerator_TencentDispatch(t *testing.T) {
	cdn := &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: "k",
	}
	gen, err := newCDNURLGenerator("VENDOR_TENCENT_COS", cdn)
	require.NoError(t, err)
	require.NotNil(t, gen)
}

// TestTencentAppID verifies the helper that derives APPID from the first
// bucket's name suffix.
func TestTencentAppID(t *testing.T) {
	cases := []struct {
		name    string
		buckets []string
		want    string
	}{
		{"standard bucket name", []string{"photos-1250000000"}, "1250000000"},
		{"multiple buckets, first wins", []string{"photos-1250000000", "videos-1250000001"}, "1250000000"},
		{"no appid suffix → empty", []string{"photos"}, ""},
		{"empty bucket list → empty", []string{}, ""},
		{"non-numeric suffix → empty", []string{"photos-prod"}, ""},
		{"trailing dash only → empty", []string{"photos-"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.ProviderConfig{}
			for _, b := range tc.buckets {
				cfg.Buckets = append(cfg.Buckets, &config.BucketConfig{Name: b})
			}
			got := tencentAppID(cfg)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestNewProvider_VolcengineWiring verifies Volcengine provider construction
// dispatches to volcengine.NewVolcengineProvider instead of returning a "not
// yet implemented" error. Endpoint is the TOS-native form (no scheme); the
// constructor only does SDK client setup — no network call — so a placeholder
// endpoint is fine. RoleARN intentionally omitted (STS is opt-in).
func TestNewProvider_VolcengineWiring(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "volc-prod",
		Vendor:    "VENDOR_VOLCENGINE_TOS",
		Endpoint:  "tos-cn-beijing.volces.com",
		Region:    "cn-beijing",
		AccessKey: "ak",
		SecretKey: "sk",
	}
	p, err := newProvider(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)

	// Provider satisfies the storage.Provider interface (compile-time check).
	var _ Provider = p
}

// TestNewCDNURLGenerator_VolcengineWiring verifies Volcengine CDN generator
// selection from the registry dispatches to volcengine.NewCDNURLGenerator.
func TestNewCDNURLGenerator_VolcengineWiring(t *testing.T) {
	cdn := &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: "k",
	}
	gen, err := newCDNURLGenerator("VENDOR_VOLCENGINE_TOS", cdn)
	require.NoError(t, err)
	require.NotNil(t, gen)
}

// TestRegistryAppByTenant pins the phase ③ tenant index: rows resolve by
// their tenant_key column, un-backfilled rows fall back to the app_key
// literal, duplicate mappings keep the first row, and MergeApp converges a
// freshly created tenant row without a full rebuild.
func TestRegistryAppByTenant(t *testing.T) {
	reg, err := NewRegistry(nil)
	require.NoError(t, err)

	mapped := &models.StorageApp{ID: 1, AppKey: "app-a", KeyPrefix: "a/", TenantKey: models.TenantKeyPtr("ten_mapped000000")}
	unmapped := &models.StorageApp{ID: 2, AppKey: "app-b", KeyPrefix: "b/"}
	dup := &models.StorageApp{ID: 3, AppKey: "app-c", KeyPrefix: "c/", TenantKey: models.TenantKeyPtr("ten_mapped000000")}
	reg.SetApps([]*models.StorageApp{mapped, unmapped, dup})

	assert.Same(t, mapped, reg.AppByTenant("ten_mapped000000"), "duplicate tenant mapping keeps the first row")
	assert.Same(t, unmapped, reg.AppByTenant("app-b"), "NULL column falls back to the app_key literal")
	assert.Nil(t, reg.AppByTenant("ten_ghost00000000"), "unknown tenant resolves nil")

	fresh := &models.StorageApp{ID: 4, AppKey: "ten_new0000000001", KeyPrefix: "ten_new0000000001/", TenantKey: models.TenantKeyPtr("ten_new0000000001")}
	reg.MergeApp(fresh)
	assert.Same(t, fresh, reg.AppByTenant("ten_new0000000001"), "MergeApp converges the tenant index without a rebuild")
}
