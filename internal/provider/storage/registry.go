package storage

import (
	"fmt"
	"strings"
	"sync"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"github.com/servekit/storage-service/internal/provider/storage/aliyun"
	"github.com/servekit/storage-service/internal/provider/storage/huawei"
	"github.com/servekit/storage-service/internal/provider/storage/s3"
	"github.com/servekit/storage-service/internal/provider/storage/tencent"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/provider/storage/volcengine"
	"github.com/servekit/storage-service/pkg/config"
)

// Registry manages storage providers and their bucket mappings.
type Registry struct {
	mu              sync.RWMutex
	providers       map[string]Provider
	buckets         map[string]*config.BucketConfig
	bucketProviders map[string]string // bucket name -> provider name
	providerConfigs map[string]*config.ProviderConfig
	cdnGenerators   map[string]types.CDNURLGenerator // bucket name -> generator; absent = CDN disabled for that bucket
}

// ProviderEntry holds provider metadata for listing.
type ProviderEntry struct {
	Name     string
	Vendor   string // raw config string, e.g. "VENDOR_ALIYUN_OSS"
	Endpoint string
	Region   string
}

// BucketEntry holds bucket metadata for listing.
type BucketEntry struct {
	Name      string
	Provider  string
	KeyPrefix string
	ACL       string
}

// NewRegistry creates a new Registry initialized from provider configs.
// Buckets are extracted from the nested provider configs. Per-bucket CDN
// generators are constructed eagerly from BucketConfig.CDN + the provider's
// vendor.
func NewRegistry(providers []*config.ProviderConfig) (*Registry, error) {
	r := &Registry{
		providers:       make(map[string]Provider),
		buckets:         make(map[string]*config.BucketConfig),
		bucketProviders: make(map[string]string),
		providerConfigs: make(map[string]*config.ProviderConfig),
		cdnGenerators:   make(map[string]types.CDNURLGenerator),
	}

	for _, pc := range providers {
		p, err := newProvider(pc)
		if err != nil {
			return nil, fmt.Errorf("create provider %q: %w", pc.Name, err)
		}
		r.providers[pc.Name] = p
		r.providerConfigs[pc.Name] = pc

		for _, bc := range pc.Buckets {
			r.buckets[bc.Name] = bc
			r.bucketProviders[bc.Name] = pc.Name

			if bc.CDN != nil {
				gen, err := newCDNURLGenerator(pc.Vendor, bc.CDN)
				if err != nil {
					return nil, fmt.Errorf("bucket %q: %w", bc.Name, err)
				}
				r.cdnGenerators[bc.Name] = gen
			}
		}
	}

	return r, nil
}

// ProviderForBucket returns the Provider for the given bucket name.
func (r *Registry) ProviderForBucket(bucket string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName, ok := r.bucketProviders[bucket]
	if !ok {
		return nil, errBucketNotFound(bucket)
	}
	p, ok := r.providers[providerName]
	if !ok {
		return nil, errProviderNotFound(providerName)
	}
	return p, nil
}

// CDNURLGeneratorForBucket returns the CDN URL generator bound to this
// bucket, or (nil, nil) if the bucket has no CDN configured (BucketConfig.CDN
// is nil). Returns an error only when the bucket itself is unknown.
//
// Each call returns the same generator instance for a given bucket (the
// generator map is populated once at NewRegistry time). Generators are
// vendor-specific (s3.CDNURLGenerator for CloudFront, aliyun.CDNURLGenerator
// for Aliyun Type A) but the caller sees them behind types.CDNURLGenerator.
func (r *Registry) CDNURLGeneratorForBucket(bucket string) (types.CDNURLGenerator, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if _, ok := r.bucketProviders[bucket]; !ok {
		return nil, errBucketNotFound(bucket)
	}
	// Absent key = CDN disabled for this bucket; (nil, nil) signals that to
	// the caller (service layer turns it into ErrCDNNotConfigured).
	return r.cdnGenerators[bucket], nil
}

// BucketConfig returns the bucket configuration for the given bucket name.
func (r *Registry) BucketConfig(name string) (*config.BucketConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bc, ok := r.buckets[name]
	if !ok {
		return nil, errBucketNotFound(name)
	}
	return bc, nil
}

// ProviderNameForBucket returns the provider name for the given bucket name.
func (r *Registry) ProviderNameForBucket(bucket string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name, ok := r.bucketProviders[bucket]
	if !ok {
		return "", errBucketNotFound(bucket)
	}
	return name, nil
}

// AllBucketNames returns all registered bucket names.
func (r *Registry) AllBucketNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.buckets))
	for name := range r.buckets {
		names = append(names, name)
	}
	return names
}

// AllProviders returns metadata for all registered providers.
func (r *Registry) AllProviders() []ProviderEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]ProviderEntry, 0, len(r.providerConfigs))
	for _, pc := range r.providerConfigs {
		entries = append(entries, ProviderEntry{
			Name:     pc.Name,
			Vendor:   pc.Vendor,
			Endpoint: pc.Endpoint,
			Region:   pc.Region,
		})
	}
	return entries
}

// AllBuckets returns metadata for all registered buckets.
func (r *Registry) AllBuckets() []BucketEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]BucketEntry, 0, len(r.buckets))
	for name, bc := range r.buckets {
		entries = append(entries, BucketEntry{
			Name:      name,
			Provider:  r.bucketProviders[name],
			KeyPrefix: bc.KeyPrefix,
			ACL:       bc.ACL,
		})
	}
	return entries
}

// VendorForBucket returns the proto Vendor of the provider that owns the bucket.
// Returns VENDOR_UNSPECIFIED if the bucket is unknown or the provider's vendor
// string doesn't map to a known enum value (defensive — config validation
// should have caught the latter at startup).
func (r *Registry) VendorForBucket(bucket string) storagev1.Vendor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerName, ok := r.bucketProviders[bucket]
	if !ok {
		return storagev1.Vendor_VENDOR_UNSPECIFIED
	}
	pc, ok := r.providerConfigs[providerName]
	if !ok {
		return storagev1.Vendor_VENDOR_UNSPECIFIED
	}
	v, ok := storagev1.Vendor_value[pc.Vendor]
	if !ok {
		return storagev1.Vendor_VENDOR_UNSPECIFIED
	}
	return storagev1.Vendor(v)
}

// --- internal helpers ---

func newProvider(cfg *config.ProviderConfig) (Provider, error) {
	v, ok := storagev1.Vendor_value[cfg.Vendor]
	if !ok {
		return nil, fmt.Errorf("unsupported vendor: %s", cfg.Vendor)
	}
	switch storagev1.Vendor(v) {
	case storagev1.Vendor_VENDOR_AWS_S3, storagev1.Vendor_VENDOR_S3_COMPATIBLE:
		p, err := s3.NewS3Provider(cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_ALIYUN_OSS:
		p, err := aliyun.NewAliyunProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN, cfg.Region)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_TENCENT_COS:
		// cos-go-sdk-v5 binds its client to ONE bucket URL at construction
		// and ignores the per-call bucket parameter on Object methods.
		// Allowing multiple buckets in one Tencent ProviderConfig would
		// silently route every request to whichever bucket is embedded in
		// the endpoint URL — reject multi-bucket configs at startup so the
		// failure is loud, not silent. Operators who need multiple COS
		// buckets must declare one ProviderConfig per bucket.
		if len(cfg.Buckets) > 1 {
			return nil, fmt.Errorf("tencent provider %q declares %d buckets but cos-go-sdk-v5 is bucket-bound; split into one provider per bucket",
				cfg.Name, len(cfg.Buckets))
		}
		// Tencent CAM STS does NOT use RoleARN — cfg.RoleARN must be empty.
		// NewTencentProvider enforces this and returns a clear error if violated.
		p, err := tencent.NewTencentProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN, cfg.Region, tencentAppID(cfg))
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_HUAWEI_OBS:
		// roleARN is the IAM agency name (委托, plain string — NOT an
		// ARN). DomainID is the Huawei account UID; the IAM global-
		// credentials builder requires it, AND it is forwarded to
		// IdentityAssumerole.DomainId inside assumeAgency to scope the
		// temp credentials to the delegating account.
		p, err := huawei.NewHuaweiProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey,
			cfg.RoleARN, cfg.DomainID, cfg.Region)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		// roleTRN is the Volcengine IAM role in TRN format
		// (trn:iam::<account-id>:role/<role-name>). Empty RoleARN disables STS
		// issuance — NewVolcengineProvider handles this; GetSTSToken returns an
		// explicit error at call time so callers know to use PresignPutObject.
		p, err := volcengine.NewVolcengineProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN, cfg.Region)
		if err != nil {
			return nil, err
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported vendor: %s", cfg.Vendor)
	}
}

// newCDNURLGenerator picks the right vendor-specific generator implementation
// for a bucket's CDNConfig. vendor is the proto enum string (e.g.
// VENDOR_ALIYUN_OSS) from the parent ProviderConfig; validateBucketCDN has
// already enforced the vendor-driven KeyPairID contract (required for
// cloudfront path, rejected elsewhere) by the time this is called.
func newCDNURLGenerator(vendor string, cdn *config.CDNConfig) (types.CDNURLGenerator, error) {
	v, ok := storagev1.Vendor_value[vendor]
	if !ok {
		return nil, fmt.Errorf("unsupported vendor: %s", vendor)
	}
	switch storagev1.Vendor(v) {
	case storagev1.Vendor_VENDOR_AWS_S3, storagev1.Vendor_VENDOR_S3_COMPATIBLE:
		return s3.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_ALIYUN_OSS:
		return aliyun.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_TENCENT_COS:
		return tencent.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_HUAWEI_OBS:
		return huawei.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return volcengine.NewCDNURLGenerator(cdn), nil
	default:
		return nil, fmt.Errorf("CDN not supported for vendor %s", vendor)
	}
}

func errBucketNotFound(bucket string) error {
	return fmt.Errorf("bucket %q not found", bucket)
}

func errProviderNotFound(provider string) error {
	return fmt.Errorf("provider %q not found", provider)
}

// tencentAppID extracts the APPID for Tencent COS from the provider config.
// The current schema has no dedicated AppID field — it must be derived from
// the bucket name suffix (Tencent bucket names embed APPID as
// "<name>-<appid>"). If the first bucket's name doesn't match the
// "<name>-<digits>" pattern, AppID is left empty and STS will be disabled
// (NewTencentProvider treats empty AppID as "no STS").
//
// This is a workaround until config schema gains an explicit appid field.
// Spec'd in docs/superpowers/specs/2026-06-25-multi-vendor-storage-providers-design.md
// (Tencent section: "bucket name MUST include APPID suffix").
func tencentAppID(cfg *config.ProviderConfig) string {
	for _, bc := range cfg.Buckets {
		if i := strings.LastIndex(bc.Name, "-"); i >= 0 {
			suffix := bc.Name[i+1:]
			if suffix != "" && isAllDigits(suffix) {
				return suffix
			}
		}
	}
	return ""
}

// isAllDigits reports whether s consists entirely of ASCII digits.
func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// NewRegistryWithProvider builds a Registry backed by a single Provider
// instance. Used by tests that need to inject a fake.FakeProvider for OSS-like
// operations (HeadObject, etc.). The provider's vendor is derived from the
// config string for VendorForBucket() to keep working.
//
// cdnGenerators optionally maps bucket name → generator; tests use it to
// inject fake.CDNURLGenerator for buckets that the test setup wants
// GetCDNURL to work against. Pass nil when CDN isn't exercised.
//
// Lives in the parent storage package (not the fake/ subpackage) because it
// populates Registry's unexported map fields directly.
func NewRegistryWithProvider(cfg *config.ProviderConfig, p Provider, cdnGenerators map[string]types.CDNURLGenerator) (*Registry, error) {
	r := &Registry{
		providers:       map[string]Provider{cfg.Name: p},
		buckets:         map[string]*config.BucketConfig{},
		bucketProviders: map[string]string{},
		providerConfigs: map[string]*config.ProviderConfig{cfg.Name: cfg},
		cdnGenerators:   make(map[string]types.CDNURLGenerator),
	}
	for _, bc := range cfg.Buckets {
		r.buckets[bc.Name] = bc
		r.bucketProviders[bc.Name] = cfg.Name
	}
	for bucket, gen := range cdnGenerators {
		r.cdnGenerators[bucket] = gen
	}
	return r, nil
}
