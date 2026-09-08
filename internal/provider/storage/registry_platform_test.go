package storage

import (
	"testing"

	"github.com/servekit/storage-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testProviderConfig(name, bucket string) *config.ProviderConfig {
	return &config.ProviderConfig{
		Name:      name,
		Vendor:    "VENDOR_S3_COMPATIBLE",
		Endpoint:  "http://localhost:9000",
		Region:    "us-east-1",
		AccessKey: "ak",
		SecretKey: "sk",
		Buckets: []*config.BucketConfig{
			{Name: bucket, ACL: "private"},
		},
	}
}

func TestRebuildSwapsContent(t *testing.T) {
	r, err := NewRegistry([]*config.ProviderConfig{testProviderConfig("p1", "b1")})
	require.NoError(t, err)

	p, err := r.ProviderForBucket("b1")
	require.NoError(t, err)
	assert.NotNil(t, p)
	assert.True(t, r.IsBucketWritable("b1"))

	// hot rebuild replaces the whole content
	require.NoError(t, r.Rebuild([]*config.ProviderConfig{testProviderConfig("p2", "b2")}, nil))
	_, err = r.ProviderForBucket("b1")
	assert.Error(t, err, "old bucket must be gone after rebuild")
	p, err = r.ProviderForBucket("b2")
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestRebuildErrorKeepsPreviousSnapshot(t *testing.T) {
	r, err := NewRegistry([]*config.ProviderConfig{testProviderConfig("p1", "b1")})
	require.NoError(t, err)

	bad := testProviderConfig("p2", "b2")
	bad.Vendor = "VENDOR_DOES_NOT_EXIST"
	err = r.Rebuild([]*config.ProviderConfig{bad}, nil)
	require.Error(t, err)

	// previous snapshot still serves
	p, err := r.ProviderForBucket("b1")
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestDisabledProviderNotWritableButReadable(t *testing.T) {
	pc := testProviderConfig("p1", "b1")
	pc.Disabled = true
	r, err := NewRegistry([]*config.ProviderConfig{pc})
	require.NoError(t, err)

	_, err = r.ProviderForBucket("b1")
	require.NoError(t, err, "client still resolvable for reads")
	assert.False(t, r.IsBucketWritable("b1"), "disabled provider must reject writes")
}

func TestSettingsSwap(t *testing.T) {
	r, err := NewRegistry(nil)
	require.NoError(t, err)
	assert.Empty(t, r.DefaultBucket())

	r.SetSettings("main", "pub")
	def, pub := r.Settings()
	assert.Equal(t, "main", def)
	assert.Equal(t, "pub", pub)
	assert.Equal(t, "main", r.DefaultBucket())
	assert.Equal(t, "pub", r.PublicBucket())
}

func TestListEntriesCarryPlatformFields(t *testing.T) {
	pc := testProviderConfig("p1", "b1")
	pc.RoleARN = "acs:ram::1:role/x"
	pc.Buckets[0].CDN = &config.CDNConfig{Domain: "cdn.example.com"}
	r, err := NewRegistry([]*config.ProviderConfig{pc})
	require.NoError(t, err)

	providers := r.AllProviders()
	require.Len(t, providers, 1)
	assert.True(t, providers[0].STSEnabled)
	assert.Equal(t, 1, providers[0].BucketCount)

	buckets := r.AllBuckets()
	require.Len(t, buckets, 1)
	assert.Equal(t, "cdn.example.com", buckets[0].CDNDomain)
}
