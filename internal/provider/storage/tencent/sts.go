package tencent

import (
	"context"
	"fmt"
	"strings"
	"time"

	cossts "github.com/tencentyun/qcloud-cos-sts-sdk/go"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// stsClient wraps the Tencent CAM STS SDK so the rest of the tencent package
// can issue GetCredential calls without exposing SDK types.
//
// Tencent CAM STS does NOT use RoleARN (unlike Aliyun/AWS AssumeRole). The
// SDK issues temp credentials directly from the supplied policy; the
// credentials' permissions are bounded by both the policy AND the IAM user
// that owns the SecretID/SecretKey (CAM takes the intersection).
type stsClient struct {
	cli    *cossts.Client
	region string
}

// stsClientOpts configures newSTSClient.
type stsClientOpts struct {
	SecretID  string
	SecretKey string
	// AppID is the Tencent Cloud APPID (numeric, e.g. "1250000000"). Required
	// because Tencent STS policy resources use the
	// "qcs::cos:<region>:uid/<appid>:<bucket-appid>/<prefix>/*" form. AppID is
	// stored for reference here but is NOT consumed by the SDK client — it is
	// only used when building the policy JSON.
	AppID string
	// Region is forwarded onto each CredentialOptions.Region field at call
	// time (the SDK does not accept Region at client construction).
	Region string
	// Host overrides the default STS API host "sts.tencentcloudapi.com".
	// Empty falls back to the SDK default. Override for internal/proxy setups.
	Host string
}

// getCredentialReq is the project-typed input for STS GetCredential.
// DurationSeconds is int64 to match the SDK field type (CAM accepts
// 1800..7200).
type getCredentialReq struct {
	Policy          map[string]any
	DurationSeconds int64
}

// getCredentialResp carries the temporary credentials. Expiration is the raw
// ISO8601 string from Tencent; callers parse it to time.Time so this package
// stays free of time-zone assumptions.
type getCredentialResp struct {
	RequestId       string
	AccessKeyId     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      string
}

// getCredentialCaller is the contract stsClient satisfies. Defining it as an
// interface lets tests inject a fake without exposing the SDK wrapper type.
type getCredentialCaller interface {
	getCredential(req *getCredentialReq) (*getCredentialResp, error)
}

const (
	// minTencentSTSDuration / maxTencentSTSDuration bound the TTL Tencent
	// CAM STS accepts on DurationSeconds. We fail fast outside this range so
	// callers get an actionable error instead of a wrapped SDK API failure.
	minTencentSTSDuration int64 = 1800
	maxTencentSTSDuration int64 = 7200
)

// newSTSClient builds a Tencent CAM STS SDK client. Returns an error on nil
// opts so callers fail fast instead of dereferencing nil later.
//
// SDK note: cossts.NewClient takes (secretId, secretKey, *http.Client,
// ...ClientOption). Host and Region are NOT constructor parameters — Host is
// a ClientOption (cossts.Host) and Region is set on each CredentialOptions at
// call time. We hold Region on the returned stsClient so callers don't have
// to repeat it.
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	clientOpts := []cossts.ClientOption{}
	if opts.Host != "" {
		clientOpts = append(clientOpts, cossts.Host(opts.Host))
	}
	c := cossts.NewClient(opts.SecretID, opts.SecretKey, nil, clientOpts...)
	return &stsClient{cli: c, region: opts.Region}, nil
}

// GetSTSToken retrieves temporary STS credentials from Tencent CAM. Unlike
// Aliyun/AWS, Tencent STS does NOT require a RoleARN — the credentials are
// issued from the supplied policy directly. p.stsCli is constructed lazily
// at NewTencentProvider time; if it's nil the provider has no STS configured.
//
// The Tencent STS SDK ignores ctx (it uses its own http.Client which has no
// context propagation); cancellation/timeout must be configured at the SDK
// level (not exposed here — add an option if needed).
func (p *TencentProvider) GetSTSToken(_ context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil {
		return nil, fmt.Errorf("tencent STS not configured for this provider; STS is opt-in at NewTencentProvider")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if err := p.checkBucket(policy.Bucket); err != nil {
		return nil, err
	}

	policyJSON, err := buildTencentPolicy(policy, p.region, p.appID)
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int64(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	// Tencent CAM enforces DurationSeconds in [1800, 7200]. Fail fast here so
	// callers get an actionable message instead of a wrapped SDK error from
	// the cloud.
	if duration < minTencentSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below Tencent CAM STS minimum of %ds",
			policy.TTL, minTencentSTSDuration)
	}
	if duration > maxTencentSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v above Tencent CAM STS maximum of %ds",
			policy.TTL, maxTencentSTSDuration)
	}

	resp, err := p.stsCli.getCredential(&getCredentialReq{
		Policy:          policyJSON,
		DurationSeconds: duration,
	})
	if err != nil {
		return nil, fmt.Errorf("tencent sts get credential: %w", err)
	}

	// Tencent STS returns Expiration as ISO8601 with timezone (e.g.
	// "2026-06-23T15:30:00Z"). Parse as RFC3339; on failure surface a clear
	// error rather than propagating a zero-time.
	expiresAt, err := time.Parse(time.RFC3339, resp.Expiration)
	if err != nil {
		return nil, fmt.Errorf("parse sts expiration %q: %w", resp.Expiration, err)
	}

	return &types.STSCredential{
		AccessKey:       resp.AccessKeyId,
		SecretKey:       resp.AccessKeySecret,
		SecurityToken:   resp.SecurityToken,
		Endpoint:        p.endpoint,
		Region:          p.region,
		Bucket:          policy.Bucket,
		ObjectKeyPrefix: policy.KeyPrefix,
		ExpiresAt:       expiresAt,
	}, nil
}

// getCredential calls STS GetCredential and maps the response to project
// types. The SDK's GetCredential accepts *cossts.CredentialOptions whose
// Policy field is a typed *cossts.CredentialPolicy struct (the SDK marshals
// it internally with json.Marshal). We translate our project-typed policy
// map into the SDK struct here so the rest of the package can keep working
// with plain maps (easier to assert on in tests).
func (c *stsClient) getCredential(req *getCredentialReq) (*getCredentialResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil get credential req")
	}
	if req.Policy == nil {
		return nil, fmt.Errorf("nil policy in get credential req")
	}
	sdkPolicy, err := toCredentialPolicy(req.Policy)
	if err != nil {
		return nil, fmt.Errorf("translate policy: %w", err)
	}
	opt := &cossts.CredentialOptions{
		Policy: sdkPolicy,
		// Region is forwarded from the client so the SDK doesn't silently
		// default to ap-guangzhou for buckets in other regions. Empty region
		// intentionally left empty here so the SDK's own default applies.
		Region:          c.region,
		DurationSeconds: req.DurationSeconds,
	}
	resp, err := c.cli.GetCredential(opt)
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	if resp == nil || resp.Credentials == nil {
		return nil, fmt.Errorf("get credential returned empty credentials")
	}
	return &getCredentialResp{
		RequestId:       resp.RequestId,
		AccessKeyId:     resp.Credentials.TmpSecretID,
		AccessKeySecret: resp.Credentials.TmpSecretKey,
		SecurityToken:   resp.Credentials.SessionToken,
		Expiration:      resp.Expiration,
	}, nil
}

// --- internal helpers ---

// buildTencentPolicy translates STSPolicy into the JSON structure expected
// by Tencent CAM STS GetCredential's Policy parameter. Returns map[string]any
// for two reasons: (a) it makes the policy trivially assertable in tests,
// and (b) it keeps this file free of SDK type dependencies so the policy
// builder can be reused outside the STS path if needed. toCredentialPolicy
// converts the map to the SDK struct at call time.
//
// Translation rules:
//   - Bucket + KeyPrefix → Resource prefix
//     "qcs::cos:<region>:uid/<appid>:<bucket-appid>/<prefix>/*"
//     region and appid are scoped when provided (non-empty); empty falls back
//     to "*" to preserve prior behavior. Tighten these to prevent a leaked
//     credential from being used against same-named buckets in other regions
//     or accounts. NOTE: Tencent bucket names already include the APPID
//     suffix (e.g. "photos-1250000000"), so the bucket segment is used as-is.
//   - AllowedExtensions (each must start with '.') → one Resource entry per ext
//   - AllowedActions defaults to ["name/cos:PostObject", "name/cos:PutObject"]
//     for credential hardening (Tencent requires the "name/cos:" prefix on
//     CAM action strings, unlike Aliyun's bare "oss:" prefix).
//   - MaxSize is intentionally NOT mapped: Tencent COS PutObject has no STS-side
//     size enforcement (only PostObject's content-length-range form field does).
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for name/cos:PutObjectACL.
//
// Tencent policy keys are lowercase: "version", "statement", "effect",
// "action", "resource", "condition" (unlike Aliyun/AWS which use Title-case).
func buildTencentPolicy(p *types.STSPolicy, region, appID string) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"name/cos:PostObject", "name/cos:PutObject"}
	}

	// Tencent bucket names embed APPID as suffix ("photos-1250000000"), so the
	// resource's bucket segment is just p.Bucket. region/appID scope the ARN
	// prefix to prevent replay against same-named buckets in other accounts.
	prefix := strings.Trim(p.KeyPrefix, "/")
	scopedResource := fmt.Sprintf("qcs::cos:%s:uid/%s:%s",
		orWildcard(region), orWildcardAppID(appID), p.Bucket)
	var base string
	if prefix == "" {
		base = scopedResource + "/*"
	} else {
		base = fmt.Sprintf("%s/%s/*", scopedResource, prefix)
	}

	var resources []string
	if len(p.AllowedExtensions) > 0 {
		for _, ext := range p.AllowedExtensions {
			if !strings.HasPrefix(ext, ".") {
				return nil, fmt.Errorf("extension %q must start with '.'", ext)
			}
			resources = append(resources, base+ext)
		}
	} else {
		resources = []string{base}
	}

	allowStmt := map[string]any{
		"effect":   "allow",
		"action":   actions,
		"resource": resources,
	}

	// Condition is only added when at least one condition is enabled — Tencent
	// CAM rejects an empty Condition block.
	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string]string{"qcs:secure_transport": "true"}
	}
	if p.LockObjectACL {
		// Tencent has no direct equivalent to Aliyun's oss:x-oss-object-acl
		// condition key in the public docs. The closest analog is enforcing
		// ACL via a Deny on PutObjectACL (which DenyPutObjectACL already
		// handles). LockObjectACL alone emits a StringLike condition on the
		// "cos:x-cos-acl" request header that requires "private"; verify in
		// the CAM console before relying on this.
		conditions["StringLike"] = map[string]string{"cos:x-cos-acl": "private"}
	}
	if len(conditions) > 0 {
		allowStmt["condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	// Explicit Deny on PutObjectACL prevents clients from changing the ACL of
	// an uploaded object to public-read. Resource matches the Allow set so
	// the deny applies exactly to what the credential could upload.
	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"effect":   "deny",
			"action":   []string{"name/cos:PutObjectACL"},
			"resource": resources,
		})
	}

	return map[string]any{
		"version":   "2.0",
		"statement": statements,
	}, nil
}

// toCredentialPolicy translates the project-typed policy map (produced by
// buildTencentPolicy) into the SDK's *cossts.CredentialPolicy struct. The SDK
// marshals the struct itself in getPolicy(), so we don't need to worry about
// HTML escaping here. Returns an error if the map shape is unexpected — this
// is a programmer error (buildTencentPolicy produced bad output), not a
// caller error.
func toCredentialPolicy(p map[string]any) (*cossts.CredentialPolicy, error) {
	version, _ := p["version"].(string)
	if version == "" {
		version = "2.0"
	}
	rawStmts, ok := p["statement"].([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("policy statement missing or wrong type %T", p["statement"])
	}
	stmts := make([]cossts.CredentialPolicyStatement, 0, len(rawStmts))
	for i, raw := range rawStmts {
		effect, _ := raw["effect"].(string)
		actions := toStringSlice(raw["action"])
		resources := toStringSlice(raw["resource"])
		if effect == "" || len(actions) == 0 || len(resources) == 0 {
			return nil, fmt.Errorf("policy statement %d missing required fields", i)
		}
		stmt := cossts.CredentialPolicyStatement{
			Effect:   effect,
			Action:   actions,
			Resource: resources,
		}
		if cond, ok := raw["condition"].(map[string]any); ok && len(cond) > 0 {
			stmt.Condition = toConditionMap(cond)
		}
		stmts = append(stmts, stmt)
	}
	return &cossts.CredentialPolicy{
		Version:   version,
		Statement: stmts,
	}, nil
}

// toStringSlice coerces an interface{} (from a map[string]any) into a
// []string. Returns nil if v is not a []string.
func toStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	if s, ok := v.([]string); ok {
		return s
	}
	// Allow []any as well — defensive; buildTencentPolicy always emits
	// []string for action/resource but being lenient here means a future
	// caller passing mixed types doesn't silently drop entries.
	if xs, ok := v.([]any); ok {
		out := make([]string, 0, len(xs))
		for _, e := range xs {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// toConditionMap coerces the condition map[string]any produced by
// buildTencentPolicy into the SDK's nested map[string]map[string]interface{}
// shape. Values are expected to be map[string]string (per buildTencentPolicy)
// but anything that json.Marshal accepts will round-trip.
func toConditionMap(cond map[string]any) map[string]map[string]interface{} {
	out := make(map[string]map[string]interface{}, len(cond))
	for op, kv := range cond {
		if kvMap, ok := kv.(map[string]string); ok {
			inner := make(map[string]interface{}, len(kvMap))
			for k, v := range kvMap {
				inner[k] = v
			}
			out[op] = inner
			continue
		}
		// Fallback: accept map[string]interface{} or map[string]any as-is by
		// copying through interface{}. Anything else is dropped (programmer
		// error in buildTencentPolicy — guard with a test).
		if kvMap, ok := kv.(map[string]interface{}); ok {
			inner := make(map[string]interface{}, len(kvMap))
			for k, v := range kvMap {
				inner[k] = v
			}
			out[op] = inner
		}
	}
	return out
}

// orWildcard returns s when non-empty, otherwise "*" — used to keep Resource
// ARN segments permissive when caller did not supply a scope.
func orWildcard(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// orWildcardAppID returns the appID string when non-empty, otherwise "*".
// Tencent resource ARN uses "uid/<appid>" as the account segment (not a
// bare ID). The "uid/" prefix is added by buildTencentPolicy's format string,
// not here, so this helper is intentionally symmetric with orWildcard.
func orWildcardAppID(appID string) string {
	if appID == "" {
		return "*"
	}
	return appID
}
