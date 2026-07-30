package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfigYAML contains a minimal but valid config with all fields specified.
const testConfigYAML = `
server:
  grpc_addr: ":9090"
  http_addr: ":8081"

database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: storage_test
  sslmode: disable

storage:
  upload_token_ttl: 15m
  upload_token_secret: "test-secret-key"
  default_quota_bytes: 5368709120
  default_bucket: "test-bucket"
  orphan_retention: 1h
  soft_delete_retention: 72h
  providers:
    - name: test-provider
      vendor: VENDOR_S3_COMPATIBLE
      endpoint: http://localhost:19093
      region: us-east-1
      access_key: minioadmin
      secret_key: minioadmin
      buckets:
        - name: test-bucket
          key_prefix: "uploads/"
          acl: private

third_party:
  gid:
    mode: grpc
    target: "localhost:19093"

log:
  level: debug
  format: text
`

// writeTestConfigFile creates a temporary YAML config file and returns its path.
func writeTestConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func TestLoadFromFile(t *testing.T) {
	cfgPath := writeTestConfigFile(t, testConfigYAML)
	t.Setenv("STORAGE_SERVICE_CONFIG", cfgPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify server section.
	if cfg.Server.GRPCAddr != ":9090" {
		t.Errorf("Server.GRPCAddr = %q, want %q", cfg.Server.GRPCAddr, ":9090")
	}
	if cfg.Server.HTTPAddr != ":8081" {
		t.Errorf("Server.HTTPAddr = %q, want %q", cfg.Server.HTTPAddr, ":8081")
	}

	// Verify database section.
	if cfg.Database.Host != "localhost" {
		t.Errorf("Database.Host = %q, want %q", cfg.Database.Host, "localhost")
	}
	if cfg.Database.Port != 5432 {
		t.Errorf("Database.Port = %d, want %d", cfg.Database.Port, 5432)
	}

	// Verify storage section.
	if cfg.Storage.UploadTokenTTL != 15*time.Minute {
		t.Errorf("Storage.UploadTokenTTL = %v, want %v", cfg.Storage.UploadTokenTTL, 15*time.Minute)
	}
	if cfg.Storage.UploadTokenSecret != "test-secret-key" {
		t.Errorf("Storage.UploadTokenSecret = %q, want %q", cfg.Storage.UploadTokenSecret, "test-secret-key")
	}
	if cfg.Storage.DefaultQuotaBytes != 5368709120 {
		t.Errorf("Storage.DefaultQuotaBytes = %d, want %d", cfg.Storage.DefaultQuotaBytes, 5368709120)
	}
	if cfg.Storage.DefaultBucket != "test-bucket" {
		t.Errorf("Storage.DefaultBucket = %q, want %q", cfg.Storage.DefaultBucket, "test-bucket")
	}
	if cfg.Storage.OrphanRetention != 1*time.Hour {
		t.Errorf("Storage.OrphanRetention = %v, want %v", cfg.Storage.OrphanRetention, 1*time.Hour)
	}
	if cfg.Storage.SoftDeleteRetention != 72*time.Hour {
		t.Errorf("Storage.SoftDeleteRetention = %v, want %v", cfg.Storage.SoftDeleteRetention, 72*time.Hour)
	}

	// Verify providers (with nested buckets).
	if len(cfg.Storage.Providers) != 1 {
		t.Fatalf("len(Storage.Providers) = %d, want %d", len(cfg.Storage.Providers), 1)
	}
	p := cfg.Storage.Providers[0]
	if p.Name != "test-provider" {
		t.Errorf("Provider.Name = %q, want %q", p.Name, "test-provider")
	}
	if p.Vendor != "VENDOR_S3_COMPATIBLE" {
		t.Errorf("Provider.Vendor = %q, want %q", p.Vendor, "VENDOR_S3_COMPATIBLE")
	}
	if len(p.Buckets) != 1 {
		t.Fatalf("len(Provider.Buckets) = %d, want %d", len(p.Buckets), 1)
	}
	b := p.Buckets[0]
	if b.Name != "test-bucket" {
		t.Errorf("Bucket.Name = %q, want %q", b.Name, "test-bucket")
	}

	// Verify log section.
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, "text")
	}
}

func TestDefaultsApplied(t *testing.T) {
	// Minimal config: only required fields, leaving defaults to kick in.
	minimalYAML := `
database:
  host: localhost
  port: 5432
  user: test
  password: test
  dbname: test
storage:
  upload_token_secret: "secret"
  default_bucket: "default"
  providers:
    - name: default
      vendor: VENDOR_S3_COMPATIBLE
      endpoint: http://localhost:19093
      region: us-east-1
      access_key: test
      secret_key: test

third_party:
  gid:
    mode: grpc
    target: "localhost:19093"
`
	cfgPath := writeTestConfigFile(t, minimalYAML)
	t.Setenv("STORAGE_SERVICE_CONFIG", cfgPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify server defaults.
	if cfg.Server.GRPCAddr != ":19093" {
		t.Errorf("Server.GRPCAddr = %q, want default %q", cfg.Server.GRPCAddr, ":19093")
	}
	if cfg.Server.HTTPAddr != ":18083" {
		t.Errorf("Server.HTTPAddr = %q, want default %q", cfg.Server.HTTPAddr, ":18083")
	}

	// Verify storage defaults.
	if cfg.Storage.UploadTokenTTL != 30*time.Minute {
		t.Errorf("Storage.UploadTokenTTL = %v, want default %v", cfg.Storage.UploadTokenTTL, 30*time.Minute)
	}
	if cfg.Storage.DefaultQuotaBytes != 10*1024*1024*1024 {
		t.Errorf("Storage.DefaultQuotaBytes = %d, want default %d", cfg.Storage.DefaultQuotaBytes, 10*1024*1024*1024)
	}
	if cfg.Storage.OrphanRetention != 2*time.Hour {
		t.Errorf("Storage.OrphanRetention = %v, want default %v", cfg.Storage.OrphanRetention, 2*time.Hour)
	}
	if cfg.Storage.SoftDeleteRetention != 168*time.Hour {
		t.Errorf("Storage.SoftDeleteRetention = %v, want default %v", cfg.Storage.SoftDeleteRetention, 168*time.Hour)
	}

	// Verify CDN runtime defaults (Task 3 added default:"1h"/"5m"/"24h" tags).
	// Pinning these through Load() catches typos like default:"1hr" that
	// Validate() tests miss (they build struct literals directly and bypass
	// the defaults engine).
	if cfg.Storage.CDN.DefaultTTL != 1*time.Hour {
		t.Errorf("Storage.CDN.DefaultTTL = %v, want default %v", cfg.Storage.CDN.DefaultTTL, 1*time.Hour)
	}
	if cfg.Storage.CDN.MinTTL != 5*time.Minute {
		t.Errorf("Storage.CDN.MinTTL = %v, want default %v", cfg.Storage.CDN.MinTTL, 5*time.Minute)
	}
	if cfg.Storage.CDN.MaxTTL != 24*time.Hour {
		t.Errorf("Storage.CDN.MaxTTL = %v, want default %v", cfg.Storage.CDN.MaxTTL, 24*time.Hour)
	}

	// Verify log defaults.
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want default %q", cfg.Log.Level, "info")
	}
	if cfg.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want default %q", cfg.Log.Format, "json")
	}
}

func TestEnvOverride(t *testing.T) {
	cfgPath := writeTestConfigFile(t, testConfigYAML)
	t.Setenv("STORAGE_SERVICE_CONFIG", cfgPath)

	// Set environment variables to override file values. The service uses
	// STORAGE_SERVICE as the env prefix (see config.go), so every key must
	// be namespaced under it.
	t.Setenv("STORAGE_SERVICE_SERVER_GRPC_ADDR", ":5000")
	t.Setenv("STORAGE_SERVICE_SERVER_HTTP_ADDR", ":6000")
	t.Setenv("STORAGE_SERVICE_STORAGE_UPLOAD_TOKEN_TTL", "10m")
	t.Setenv("STORAGE_SERVICE_LOG_LEVEL", "error")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Verify env overrides take effect.
	if cfg.Server.GRPCAddr != ":5000" {
		t.Errorf("Server.GRPCAddr = %q, want %q (env override)", cfg.Server.GRPCAddr, ":5000")
	}
	if cfg.Server.HTTPAddr != ":6000" {
		t.Errorf("Server.HTTPAddr = %q, want %q (env override)", cfg.Server.HTTPAddr, ":6000")
	}
	if cfg.Storage.UploadTokenTTL != 10*time.Minute {
		t.Errorf("Storage.UploadTokenTTL = %v, want %v (env override)", cfg.Storage.UploadTokenTTL, 10*time.Minute)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("Log.Level = %q, want %q (env override)", cfg.Log.Level, "error")
	}
}

// TestExpandEnv verifies ${VAR} placeholders in the config file are expanded
// from the environment — including inside the providers slice. Each provider
// account references its own credential via a distinct variable name, which is
// how multiple same-vendor accounts (e.g. two Aliyun accounts) coexist under
// static config without colliding.
func TestExpandEnv(t *testing.T) {
	yaml := `
storage:
  upload_token_secret: ${UPLOAD_TOKEN_SECRET}
  default_bucket: "default"
  providers:
    - name: aliyun-main
      vendor: VENDOR_ALIYUN_OSS
      access_key: ${ALIYUN_MAIN_AK}
      secret_key: ${ALIYUN_MAIN_SK}
    - name: aliyun-backup
      vendor: VENDOR_ALIYUN_OSS
      access_key: ${ALIYUN_BACKUP_AK}
      secret_key: ${ALIYUN_BACKUP_SK}

third_party:
  gid:
    mode: grpc
    target: "localhost:19093"
`
	cfgPath := writeTestConfigFile(t, yaml)
	t.Setenv("STORAGE_SERVICE_CONFIG", cfgPath)
	t.Setenv("UPLOAD_TOKEN_SECRET", "expanded-token-secret")
	t.Setenv("ALIYUN_MAIN_AK", "main-ak")
	t.Setenv("ALIYUN_MAIN_SK", "main-sk")
	t.Setenv("ALIYUN_BACKUP_AK", "backup-ak")
	t.Setenv("ALIYUN_BACKUP_SK", "backup-sk")

	cfg, err := Load()
	require.NoError(t, err)

	// Top-level scalar expansion.
	assert.Equal(t, "expanded-token-secret", cfg.Storage.UploadTokenSecret)

	// Per-account expansion inside the providers slice: same vendor, distinct
	// credentials resolved via distinct env var names.
	require.Len(t, cfg.Storage.Providers, 2)
	assert.Equal(t, "main-ak", cfg.Storage.Providers[0].AccessKey)
	assert.Equal(t, "main-sk", cfg.Storage.Providers[0].SecretKey)
	assert.Equal(t, "backup-ak", cfg.Storage.Providers[1].AccessKey)
	assert.Equal(t, "backup-sk", cfg.Storage.Providers[1].SecretKey)
}

// TestSTSConfig_Validate covers the consistency rules enforced at startup:
// DefaultTTL <= MaxTTL, SafetyMargin < MaxTTL, ReadRefreshWindow < SafetyMargin.
// Each rule must fail-fast with an error naming the offending fields.
func TestSTSConfig_Validate(t *testing.T) {
	valid := func() *STSConfig {
		return &STSConfig{
			DefaultTTL:        15 * time.Minute,
			MaxTTL:            1 * time.Hour,
			SafetyMargin:      5 * time.Minute,
			ReadRefreshWindow: 30 * time.Second,
		}
	}

	t.Run("valid config returns nil", func(t *testing.T) {
		assert.NoError(t, valid().validate())
	})

	t.Run("nil receiver returns nil", func(t *testing.T) {
		var nilCfg *STSConfig
		assert.NoError(t, nilCfg.validate())
	})

	t.Run("DefaultTTL > MaxTTL rejected", func(t *testing.T) {
		cfg := valid()
		cfg.DefaultTTL = 2 * time.Hour
		cfg.MaxTTL = 1 * time.Hour
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default_ttl")
		assert.Contains(t, err.Error(), "max_ttl")
	})

	t.Run("SafetyMargin >= MaxTTL rejected (would always produce negative cacheTTL)", func(t *testing.T) {
		cfg := valid()
		cfg.SafetyMargin = 1 * time.Hour
		cfg.MaxTTL = 1 * time.Hour
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "safety_margin")
		assert.Contains(t, err.Error(), "max_ttl")
	})

	t.Run("ReadRefreshWindow >= SafetyMargin rejected (read stricter than write)", func(t *testing.T) {
		cfg := valid()
		cfg.ReadRefreshWindow = 5 * time.Minute
		cfg.SafetyMargin = 5 * time.Minute
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read_refresh_window")
		assert.Contains(t, err.Error(), "safety_margin")
	})

	t.Run("zero SafetyMargin rejected", func(t *testing.T) {
		cfg := valid()
		cfg.SafetyMargin = 0
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "safety_margin")
	})

	t.Run("zero ReadRefreshWindow rejected", func(t *testing.T) {
		cfg := valid()
		cfg.ReadRefreshWindow = 0
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read_refresh_window")
	})

	t.Run("short TTL config valid when margins scale down proportionally", func(t *testing.T) {
		// Users running with MaxTTL=3m must configure SafetyMargin < 3m and
		// ReadRefreshWindow < SafetyMargin. Validate must accept this.
		cfg := &STSConfig{
			DefaultTTL:        3 * time.Minute,
			MaxTTL:            3 * time.Minute,
			SafetyMargin:      30 * time.Second,
			ReadRefreshWindow: 5 * time.Second,
		}
		assert.NoError(t, cfg.validate())
	})
}

// TestRoleARN_OptionalButValidatedForAliyun verifies RoleARN is optional:
// Config.Validate accepts a provider with empty RoleARN regardless of vendor
// (STS-unavailable is a runtime concern surfaced by GetSTSToken, not a
// config-time error). This case uses an Aliyun provider as one example;
// S3/MinIO providers follow the same rule. Pins the design so a future
// commit adding startup RoleARN validation fails this test loudly.
func TestRoleARN_OptionalButValidatedForAliyun(t *testing.T) {
	cfg := &Config{
		Storage: &StorageConfig{
			UploadTokenSecret: "test-secret",
			DefaultBucket:     "photos",
			Providers: []*ProviderConfig{{
				Name:      "aliyun-prod",
				Vendor:    "VENDOR_ALIYUN_OSS",
				Endpoint:  "oss-cn-hangzhou.aliyuncs.com",
				AccessKey: "ak",
				SecretKey: "sk",
				Buckets:   []*BucketConfig{{Name: "photos"}},
				// RoleARN intentionally empty — STS unavailable, but config still valid.
			}},
			STS: &STSConfig{
				DefaultTTL:        15 * time.Minute,
				MaxTTL:            1 * time.Hour,
				SafetyMargin:      5 * time.Minute,
				ReadRefreshWindow: 30 * time.Second,
			},
			CDN: CDNRuntimeConfig{
				DefaultTTL: 1 * time.Hour,
				MinTTL:     5 * time.Minute,
				MaxTTL:     24 * time.Hour,
			},
		},
		ThirdParty: &ThirdPartyConfig{
			GID: &RemoteServiceConfig[*gidconfig.Config]{Mode: "grpc", Target: "localhost:19093"},
		},
	}
	assert.NoError(t, cfg.Validate(),
		"empty RoleARN is valid config; STS-unavailable is a runtime concern, not config")
}

// --- CDN config tests ---

// validConfig returns a Config that passes Validate with no CDN configured
// on any provider (CDN disabled by default).
func validConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Storage: &StorageConfig{
			UploadTokenSecret: "secret",
			DefaultBucket:     "photos",
			Providers: []*ProviderConfig{{
				Name:      "p",
				Vendor:    "VENDOR_S3_COMPATIBLE",
				Endpoint:  "http://localhost:19093",
				Region:    "us-east-1",
				AccessKey: "ak",
				SecretKey: "sk",
				Buckets:   []*BucketConfig{{Name: "photos"}},
			}},
			STS: &STSConfig{
				DefaultTTL:        15 * time.Minute,
				MaxTTL:            1 * time.Hour,
				SafetyMargin:      5 * time.Minute,
				ReadRefreshWindow: 30 * time.Second,
			},
			CDN: CDNRuntimeConfig{
				DefaultTTL: 1 * time.Hour,
				MinTTL:     5 * time.Minute,
				MaxTTL:     24 * time.Hour,
			},
		},
		ThirdParty: &ThirdPartyConfig{
			GID: &RemoteServiceConfig[*gidconfig.Config]{Mode: "grpc", Target: "localhost:19093"},
		},
	}
}

// validConfigWithCDN returns validConfig() with the given CDN attached to
// the first provider's first bucket. Vendor is switched to match the CDN's
// needs:
//   - When KeyPairID is non-empty (cloudfront path): vendor → VENDOR_AWS_S3
//   - Otherwise: vendor stays as the default VENDOR_S3_COMPATIBLE only if
//     the test doesn't pre-set one. For Aliyun-style tests (no KeyPairID),
//     caller must set VENDOR_ALIYUN_OSS explicitly before calling.
func validConfigWithCDN(t *testing.T, cdn *CDNConfig) *Config {
	t.Helper()
	cfg := validConfig(t)
	cfg.Storage.Providers[0].Buckets[0].CDN = cdn
	if cdn.KeyPairID != "" {
		// cloudfront path needs AWS / S3-compatible vendor
		cfg.Storage.Providers[0].Vendor = "VENDOR_AWS_S3"
	}
	return cfg
}

// TestCDNConfig_Validate_DomainRequired verifies that setting CDN with an
// empty Domain fails Validate.
func TestCDNConfig_Validate_DomainRequired(t *testing.T) {
	cfg := validConfigWithCDN(t, &CDNConfig{AuthKey: "k"})
	cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cdn.domain is required")
}

// TestCDNConfig_Validate_BadDomainFormat verifies domain format checks.
func TestCDNConfig_Validate_BadDomainFormat(t *testing.T) {
	cases := []string{
		"https://cdn.example.com", // scheme
		"cdn.example.com/",        // trailing slash
		"localhost",               // no dot
	}
	for _, domain := range cases {
		cfg := validConfigWithCDN(t, &CDNConfig{Domain: domain, AuthKey: "k"})
		cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
		err := cfg.Validate()
		require.Error(t, err, "domain %q should be rejected", domain)
		assert.Contains(t, err.Error(), "cdn.domain")
	}
}

// TestCDNConfig_Validate_MissingAuthKey verifies AuthKey required.
func TestCDNConfig_Validate_MissingAuthKey(t *testing.T) {
	cfg := validConfigWithCDN(t, &CDNConfig{Domain: "cdn.example.com"})
	cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_key")
}

// TestCDNConfig_Validate_CloudFrontRequiresKeyPairID verifies CloudFront
// needs KeyPairID.
func TestCDNConfig_Validate_CloudFrontRequiresKeyPairID(t *testing.T) {
	cfg := validConfigWithCDN(t, &CDNConfig{Domain: "cdn.example.com", AuthKey: "/path/to/key.pem"})
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_pair_id")
}

// TestCDNConfig_Validate_ValidAliyun verifies a fully-formed Aliyun config passes.
func TestCDNConfig_Validate_ValidAliyun(t *testing.T) {
	cfg := validConfigWithCDN(t, &CDNConfig{Domain: "cdn.example.com", AuthKey: "the-key"})
	cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
	assert.NoError(t, cfg.Validate())
}

// TestCDNConfig_Validate_ValidCloudFront verifies a fully-formed CloudFront
// config (with KeyPairID) passes.
func TestCDNConfig_Validate_ValidCloudFront(t *testing.T) {
	cfg := validConfigWithCDN(t, &CDNConfig{
		Domain:    "cdn.example.com",
		AuthKey:   "/path/to/key.pem",
		KeyPairID: "APKAJXXXX",
	})
	assert.NoError(t, cfg.Validate())
}

// TestCDNConfig_Validate_DisabledByDefault verifies a provider with CDN=nil
// is accepted (CDN is opt-in).
func TestCDNConfig_Validate_DisabledByDefault(t *testing.T) {
	cfg := validConfig(t)
	assert.NoError(t, cfg.Validate())
}

// TestCDNConfig_Validate_KeyPairIDVendorMismatch verifies KeyPairID is
// rejected on non-cloudfront vendors and required on cloudfront vendors.
// Replaces the deleted AuthType consistency check.
func TestCDNConfig_Validate_KeyPairIDVendorMismatch(t *testing.T) {
	t.Run("KeyPairID on Aliyun rejected", func(t *testing.T) {
		cfg := validConfigWithCDN(t, &CDNConfig{
			Domain:    "cdn.example.com",
			AuthKey:   "k",
			KeyPairID: "APKAJXXXX",
		})
		cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key_pair_id is not used by vendor")
	})

	t.Run("KeyPairID missing on cloudfront rejected", func(t *testing.T) {
		cfg := validConfigWithCDN(t, &CDNConfig{
			Domain:  "cdn.example.com",
			AuthKey: "/path/to/key.pem",
			// KeyPairID intentionally empty
		})
		// Vendor defaults to VENDOR_S3_COMPATIBLE in validConfig
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key_pair_id is required for vendor")
	})
}

// TestCDNRuntimeConfig_Validate_TTLBounds verifies TTL ordering.
func TestCDNRuntimeConfig_Validate_TTLBounds(t *testing.T) {
	t.Run("min > default rejected", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Storage.CDN = CDNRuntimeConfig{MinTTL: 2 * time.Hour, DefaultTTL: time.Hour, MaxTTL: 3 * time.Hour}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "min_ttl")
	})
	t.Run("default > max rejected", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Storage.CDN = CDNRuntimeConfig{MinTTL: 5 * time.Minute, DefaultTTL: 2 * time.Hour, MaxTTL: time.Hour}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default_ttl")
	})
	t.Run("zero max rejected", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Storage.CDN = CDNRuntimeConfig{MinTTL: 5 * time.Minute, DefaultTTL: time.Hour, MaxTTL: 0}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_ttl")
	})
}

// loadEnvFile parses a dotenv file and sets each KEY=VALUE into the test
// environment. Mirrors docker-compose `env_file` semantics: blank lines and
// lines starting with '#' are skipped, inline comments are NOT supported (the
// value is everything after the first '='), and quotes are not stripped.
func loadEnvFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read env file: %s", path)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		require.NotEqual(t, -1, idx, "malformed env line (no '='): %q", line)
		key := strings.TrimSpace(line[:idx])
		require.NotEmpty(t, key, "empty key in env line: %q", line)
		t.Setenv(key, line[idx+1:])
	}
}

// TestExampleConfigsAreLoadable guards that the committed example files stay
// self-consistent: it loads config.example.yaml with every ${VAR} resolved
// from .env.example and asserts Load() + Validate() succeed with real
// (expanded) values. Catches drift in either direction — a ${VAR} in the YAML
// with no matching var in .env.example, or an env value that breaks parsing.
func TestExampleConfigsAreLoadable(t *testing.T) {
	root := filepath.Join("..", "..")
	yamlPath := filepath.Join(root, "config.example.yaml")
	envPath := filepath.Join(root, ".env.example")

	loadEnvFile(t, envPath)
	t.Setenv("STORAGE_SERVICE_CONFIG", yamlPath)

	cfg, err := Load()
	require.NoError(t, err, "config.example.yaml + .env.example must load and validate")

	// Spot-check that ${VAR} was actually expanded, not left as a literal.
	assert.Equal(t, ":19093", cfg.Server.GRPCAddr)
	assert.Equal(t, "default", cfg.Storage.DefaultBucket)
	assert.NotEqual(t, "${STORAGE_UPLOAD_TOKEN_SECRET}", cfg.Storage.UploadTokenSecret,
		"env expansion must have replaced the placeholder")
	require.Len(t, cfg.Storage.Providers, 2)
	assert.Equal(t, "aliyun-primary", cfg.Storage.Providers[0].Name)
	assert.Equal(t, "VENDOR_AWS_S3", cfg.Storage.Providers[1].Vendor)
	assert.Equal(t, int64(10*1024*1024*1024), cfg.Storage.DefaultQuotaBytes)
}
