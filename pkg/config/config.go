package config

import (
	"fmt"
	"strings"
	"time"

	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/ratelimit"
	"github.com/servekit/go-common/redisx"
)

// serviceName identifies this binary in config file lookup (/etc/<name>) and
// the <NAME>_CONFIG env var. envPrefix scopes all env overrides under
// STORAGE_SERVICE_*.
const (
	serviceName = "storage-service"
	envPrefix   = "STORAGE_SERVICE"
)

// Config holds all configuration for the storage service.
//
// Per golang-development §14, sub-config fields are pointers: consistency with
// Load()'s *Config return, alignment with functional-options style, and
// cheaper copy semantics. Field access via . is unchanged (Go auto-derefs a
// single layer of pointer).
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Redis      *redisx.Config
	Storage    *StorageConfig
	ThirdParty *ThirdPartyConfig
	Log        *logging.Config
}

// ThirdPartyConfig holds third-party service connection settings.
type ThirdPartyConfig struct {
	GID *RemoteServiceConfig[*gidconfig.Config]
}

// RemoteServiceConfig holds connection settings for a service that can run
// in-process (module) or as a remote gRPC deployment. T is the full config
// used in module mode. Mode has no default — Validate below decides how to
// treat an empty value.
type RemoteServiceConfig[T any] struct {
	Mode   string // "module" | "grpc"
	Target string // gRPC addr, e.g. "localhost:19091"
	Config T      // module-mode config
}

// ServerConfig holds gRPC and HTTP server addresses.
type ServerConfig struct {
	GRPCAddr string `default:":19093"`
	HTTPAddr string `default:":18083"`
}

// StorageConfig holds storage backend settings including providers and their buckets.
type StorageConfig struct {
	UploadTokenTTL        time.Duration `default:"30m"`
	UploadTokenSecret     string
	DefaultQuotaBytes     int64 `default:"10737418240"` // 10GB
	DefaultBucket         string
	OrphanRetention       time.Duration `default:"2h"`
	SoftDeleteRetention   time.Duration `default:"168h"` // 7 days
	DeletedOwnerRetention time.Duration `default:"720h"` // 30 days
	RateLimit             *ratelimit.Config
	Providers             []*ProviderConfig
	STS                   *STSConfig
	UploadGC              *UploadGCConfig
	Batch                 *BatchConfig
	Cron                  *CronConfig
	UploadSession         *UploadSessionConfig
	CDN                   CDNRuntimeConfig
}

// UploadSessionConfig configures session TTL and the shared upload lock
// (used by session dedup, object dedup, and GC reap — see upload.NewLock).
type UploadSessionConfig struct {
	TTL  time.Duration `default:"15m"`
	Lock *LockConfig
}

// CDNRuntimeConfig sits at Storage level (not per-provider) — TTL defaults
// and limits that apply to every GenerateCDNURL call regardless of provider.
// Defaults: 1h TTL, 5m floor, 24h ceiling. Validate enforces
// MinTTL <= DefaultTTL <= MaxTTL.
type CDNRuntimeConfig struct {
	DefaultTTL time.Duration `default:"1h"`
	MinTTL     time.Duration `default:"5m"`
	MaxTTL     time.Duration `default:"24h"`
}

// LockConfig configures a redisx.Lock instance.
type LockConfig struct {
	Prefix string        `default:"upload"`
	TTL    time.Duration `default:"10s"`
	Tries  int           `default:"3"`
	Wait   time.Duration `default:"100ms"`
}

// STSConfig configures STS credential caching.
type STSConfig struct {
	DefaultTTL time.Duration `default:"15m"`
	MaxTTL     time.Duration `default:"1h"`
	// SafetyMargin is subtracted from a credential's remaining lifetime when
	// computing the cache TTL, so a cache hit hands out a credential with at
	// least this much usable life at the cloud provider. Trades cache lifetime
	// for a hard guarantee that callers can actually use what they receive —
	// without it, a cache hit near expiry returns a token with seconds-to-live
	// and in-flight uploads get rejected ("SecurityToken expired").
	// Must be strictly less than MaxTTL (validated at startup).
	SafetyMargin time.Duration `default:"5m"`
	// ReadRefreshWindow is the read-side last line of defense: a cached
	// credential with remaining lifetime at or below this window is treated as
	// a miss and re-issued. Covers wall-clock drift and edge cases the
	// write-side SafetyMargin clamp can't catch (e.g. issuer returned a cred
	// shorter than SafetyMargin). Must be strictly less than SafetyMargin
	// (validated at startup), otherwise read would reject every cred the
	// write side considered cacheable.
	ReadRefreshWindow time.Duration `default:"30s"`
	Lock              *STSLockConfig
}

// STSLockConfig configures the singleflight dedup lock for STS credential
// issuance. Defaults reflect what was hardcoded before externalization.
type STSLockConfig struct {
	Prefix string        `default:"sts:lock"`
	TTL    time.Duration `default:"10s"`
	Tries  int           `default:"3"`
	Wait   time.Duration `default:"50ms"`
}

// validate checks STSConfig internal consistency. Returns nil if STS is not
// configured (nil receiver); otherwise enforces:
//   - DefaultTTL <= MaxTTL   (ResolveTTL's contract depends on this)
//   - SafetyMargin < MaxTTL  (else cacheTTL = credTTL - SafetyMargin is always negative)
//   - ReadRefreshWindow < SafetyMargin (else read rejects every cred write considered cacheable)
//
// Each rule's error message names the offending field paths so operators can
// fix the config file without reading code.
func (s *STSConfig) validate() error {
	if s == nil {
		return nil
	}
	if s.DefaultTTL <= 0 {
		return fmt.Errorf("storage.sts.default_ttl must be > 0")
	}
	if s.MaxTTL <= 0 {
		return fmt.Errorf("storage.sts.max_ttl must be > 0")
	}
	if s.DefaultTTL > s.MaxTTL {
		return fmt.Errorf("storage.sts.default_ttl (%v) must be <= storage.sts.max_ttl (%v)",
			s.DefaultTTL, s.MaxTTL)
	}
	if s.SafetyMargin <= 0 {
		return fmt.Errorf("storage.sts.safety_margin must be > 0")
	}
	if s.SafetyMargin >= s.MaxTTL {
		return fmt.Errorf("storage.sts.safety_margin (%v) must be < storage.sts.max_ttl (%v); "+
			"otherwise cache TTL is always negative and STS is effectively disabled",
			s.SafetyMargin, s.MaxTTL)
	}
	if s.ReadRefreshWindow <= 0 {
		return fmt.Errorf("storage.sts.read_refresh_window must be > 0")
	}
	if s.ReadRefreshWindow >= s.SafetyMargin {
		return fmt.Errorf("storage.sts.read_refresh_window (%v) must be < storage.sts.safety_margin (%v); "+
			"otherwise read() rejects every credential the write side considered cacheable",
			s.ReadRefreshWindow, s.SafetyMargin)
	}
	return nil
}

// UploadGCConfig configures the periodic orphan GC.
type UploadGCConfig struct {
	CronSpec  string `default:"*/5 * * * *"`
	BatchSize int    `default:"100"`
}

// BatchConfig configures BatchGetSTSCredential limits.
type BatchConfig struct {
	MaxSize     int `default:"100"`
	Concurrency int `default:"10"`
}

// CronConfig configures the internal cronx instance (used by GC).
type CronConfig struct {
	Timezone string `default:"Asia/Shanghai"`
}

// CDNConfig configures CDN signing for a single bucket. nil on BucketConfig
// means CDN is disabled for that bucket (GenerateCDNURL returns
// ErrCDNNotConfigured).
//
// CDN in the cloud-vendor sense is per-bucket: Aliyun CDN's origin is one
// specific OSS bucket; CloudFront's distribution origin is one specific S3
// bucket. Keeping the config at bucket level (rather than provider level)
// matches reality and lets two buckets under the same provider use different
// CDN domains.
//
// Generator selection is by parent ProviderConfig.Vendor (not by an explicit
// auth-type field). KeyPairID is only meaningful for the cloudfront path
// (VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE); validateBucketCDN enforces this.
type CDNConfig struct {
	// Domain is a bare hostname (e.g. cdn.example.com). No scheme, no path,
	// no trailing slash — validateCDNDomain enforces this. The URL scheme is
	// always https (see types.SchemeHTTPS); http CDN distribution is not
	// supported.
	Domain string
	// AuthKey is the signing key. Semantics depend on vendor:
	//   - Aliyun/Huawei/Volcengine: literal primary key from CDN console
	//     (used in MD5 auth_key / equivalent).
	//   - AWS S3 / S3-compatible (cloudfront): file path to a PEM private key.
	AuthKey string
	// KeyPairID is the CloudFront key pair ID (required only for the
	// cloudfront path, i.e. VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE).
	// validateBucketCDN rejects non-empty KeyPairID on other vendors and
	// empty KeyPairID on cloudfront vendors.
	KeyPairID string
}

// ProviderConfig defines a storage provider (e.g., Aliyun OSS, AWS S3) and its buckets.
type ProviderConfig struct {
	Name      string
	Vendor    string // proto Vendor enum name, e.g. VENDOR_ALIYUN_OSS / VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	// RoleARN is the IAM/RAM role reference for STS AssumeRole. Format and
	// required-ness vary by vendor:
	//
	//   - VENDOR_ALIYUN_OSS:     "acs:ram::<account-id>:role/<role-name>"
	//                            (required for STS; empty = STS unavailable)
	//   - VENDOR_AWS_S3:         "arn:aws:iam::<account-id>:role/<role-name>"
	//                            (required for STS; empty = STS unavailable)
	//   - VENDOR_S3_COMPATIBLE:  any non-empty identifier — MinIO doesn't
	//                            validate the format (optional)
	//   - VENDOR_TENCENT_COS:    UNUSED — Tencent CAM STS issues temp
	//                            credentials directly from policy without
	//                            a role. Leave empty.
	//   - VENDOR_HUAWEI_OBS:     agency name (委托名, plain string, NOT an
	//                            ARN) from IAM console. Required for STS.
	//   - VENDOR_VOLCENGINE_TOS: "trn:iam::<account-id>:role/<role-name>".
	//                            Required for STS.
	//
	// Format validation is per-vendor at provider construction time
	// (NewXxxProvider returns error on malformed non-empty RoleARN). Empty
	// = STS unavailable for this provider; clients must use GenerateUploadURL.
	RoleARN string
	// DomainID is the Huawei Cloud account UID (numeric). Required only by
	// VENDOR_HUAWEI_OBS — Huawei's IAM global-credentials builder needs the
	// domain ID to issue CreateTemporaryAccessKeyByAgency tokens. Empty on
	// all other vendors (ignored by their SDKs).
	DomainID string
	Buckets  []*BucketConfig
}

// BucketConfig defines a storage bucket.
type BucketConfig struct {
	Name      string
	KeyPrefix string
	ACL       string
	// CDN is optional; nil = CDN disabled for this bucket. When set,
	// Validate enforces Domain/AuthKey (and KeyPairID for cloudfront vendors).
	CDN *CDNConfig
}

// Load reads configuration from file and environment, expands ${VAR}
// references in the file against the environment, applies defaults, then
// validates and returns a Config.
//
// Env expansion lets config.yaml reference secrets by name
// (e.g. access_key: ${ALIYUN_AK}) instead of holding the literal value, so
// multiple same-vendor accounts each resolve their own credential via a
// distinct variable name. Unset vars expand to "" (os.ExpandEnv semantics),
// which Validate then surfaces as a missing-required-field error.
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName(serviceName),
		configx.WithEnvPrefix(envPrefix),
		configx.WithExpandEnv(),
	); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that all required configuration fields are set.
func (c *Config) Validate() error {
	if c.Storage.UploadTokenSecret == "" {
		return fmt.Errorf("storage.upload_token_secret is required")
	}
	if c.Storage.DefaultBucket == "" {
		return fmt.Errorf("storage.default_bucket is required")
	}
	if len(c.Storage.Providers) == 0 {
		return fmt.Errorf("at least one storage provider is required")
	}
	for i, p := range c.Storage.Providers {
		if p.Name == "" {
			return fmt.Errorf("storage.providers[%d].name is required", i)
		}
		if p.Vendor == "" {
			return fmt.Errorf("storage.providers[%d].vendor is required", i)
		}
		for j, b := range p.Buckets {
			if b.Name == "" {
				return fmt.Errorf("storage.providers[%d].buckets[%d].name is required", i, j)
			}
			if b.CDN != nil {
				if err := validateBucketCDN(i, j, b.CDN, p.Vendor); err != nil {
					return err
				}
			}
		}
	}
	if err := c.Storage.STS.validate(); err != nil {
		return err
	}
	if c.Storage.CDN.MaxTTL <= 0 {
		return fmt.Errorf("storage.cdn.max_ttl must be > 0")
	}
	if c.Storage.CDN.DefaultTTL <= 0 {
		return fmt.Errorf("storage.cdn.default_ttl must be > 0")
	}
	if c.Storage.CDN.MinTTL <= 0 {
		return fmt.Errorf("storage.cdn.min_ttl must be > 0")
	}
	if c.Storage.CDN.MinTTL > c.Storage.CDN.DefaultTTL {
		return fmt.Errorf("storage.cdn.min_ttl (%v) must be <= default_ttl (%v)", c.Storage.CDN.MinTTL, c.Storage.CDN.DefaultTTL)
	}
	if c.Storage.CDN.DefaultTTL > c.Storage.CDN.MaxTTL {
		return fmt.Errorf("storage.cdn.default_ttl (%v) must be <= max_ttl (%v)", c.Storage.CDN.DefaultTTL, c.Storage.CDN.MaxTTL)
	}
	if c.ThirdParty.GID.Mode == "" {
		return fmt.Errorf("third_party.gid.mode is required (module or grpc)")
	}
	switch c.ThirdParty.GID.Mode {
	case "module":
		// Module mode runs gid-service in-process. A parent that embeds this
		// service may inject a Handler via option.WithGIDHandler (then Config
		// is unused); otherwise standalone (cmd/server) builds one from Config.
		// Snowflake field validation is delegated to gidservice.NewModule
		// (→ gidconfig.ValidateSnowflake) at build time.
		if c.ThirdParty.GID.Config == nil {
			return fmt.Errorf("third_party.gid.config is required for module mode")
		}
	case "grpc":
		if c.ThirdParty.GID.Target == "" {
			return fmt.Errorf("third_party.gid.target is required for grpc mode")
		}
	default:
		return fmt.Errorf("third_party.gid.mode must be module or grpc, got %q", c.ThirdParty.GID.Mode)
	}
	return nil
}

// --- internal helpers ---

// validateCDNDomain ensures the domain is a bare hostname suitable for
// url.URL{Host: ...}. Rejects schemes, paths, and missing dots.
func validateCDNDomain(d string) error {
	if strings.HasPrefix(strings.ToLower(d), "http://") || strings.HasPrefix(strings.ToLower(d), "https://") {
		return fmt.Errorf("must not include scheme (got %q)", d)
	}
	if strings.HasSuffix(d, "/") {
		return fmt.Errorf("must not end with / (got %q)", d)
	}
	if !strings.Contains(d, ".") {
		return fmt.Errorf("must contain at least one dot (got %q)", d)
	}
	return nil
}

// validateBucketCDN checks a single bucket's CDNConfig. vendor is the parent
// provider's vendor string (e.g. VENDOR_ALIYUN_OSS).
//
// KeyPairID requirement is vendor-driven (formerly driven by AuthType):
//   - VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE (cloudfront path): KeyPairID required
//   - All other vendors: KeyPairID must be empty (the field is meaningless)
//
// Path prefix reflects the bucket-level config location:
// storage.providers[i].buckets[j].cdn.
func validateBucketCDN(i, j int, cdn *CDNConfig, vendor string) error {
	if cdn.Domain == "" {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.domain is required when cdn is set", i, j)
	}
	if err := validateCDNDomain(cdn.Domain); err != nil {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.domain: %w", i, j, err)
	}
	if cdn.AuthKey == "" {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.auth_key is required when cdn is set", i, j)
	}
	switch vendor {
	case "VENDOR_AWS_S3", "VENDOR_S3_COMPATIBLE":
		if cdn.KeyPairID == "" {
			return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.key_pair_id is required for vendor %q (cloudfront signing)", i, j, vendor)
		}
	default:
		if cdn.KeyPairID != "" {
			return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.key_pair_id is not used by vendor %q (only cloudfront path needs it)", i, j, vendor)
		}
	}
	return nil
}
