package aliyun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	sts "github.com/alibabacloud-go/sts-20150401/v2/client"
	tea "github.com/alibabacloud-go/tea/tea"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// stsClient wraps the Aliyun STS SDK so the rest of the aliyun package can
// issue AssumeRole calls without exposing SDK types.
type stsClient struct {
	cli *sts.Client
}

// stsClientOpts configures newSTSClient. Timeouts are pointers so callers can
// pass nil to keep SDK defaults; non-nil values are in milliseconds.
type stsClientOpts struct {
	AccessKeyId     string
	AccessKeySecret string
	RegionId        string
	Endpoint        string
	// Protocol sets the wire scheme the SDK uses when composing request URLs
	// (final URL = Protocol + "://" + Endpoint). Empty falls back to the SDK
	// default ("https"). Set to "http" when targeting an httptest.Server or a
	// self-hosted STS-compatible endpoint that only exposes plaintext.
	Protocol       string
	ConnectTimeout *int
	ReadTimeout    *int
}

// assumeRoleReq is the project-typed input for AssumeRole. DurationSeconds is
// int64 to match the SDK field type (Aliyun accepts 900..MaxSessionDuration).
type assumeRoleReq struct {
	RoleArn         string
	RoleSessionName string
	DurationSeconds *int64
	Policy          map[string]any
}

// assumeRoleResp carries the temporary credentials. Expiration is the raw
// ISO8601 string from Aliyun; callers parse it to time.Time so this package
// stays free of time-zone assumptions.
type assumeRoleResp struct {
	RequestId       string
	AccessKeyId     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      string
}

// assumeRoleCaller is the contract stsClient satisfies. Defining it as an
// interface lets tests inject a fake without exposing the SDK wrapper type.
type assumeRoleCaller interface {
	assumeRole(req *assumeRoleReq) (*assumeRoleResp, error)
}

const (
	// minAliyunSTSDuration is the lower bound Aliyun AssumeRole enforces on
	// DurationSeconds. We fail fast below this so callers get an actionable
	// error instead of a wrapped SDK API failure.
	minAliyunSTSDuration int64 = 900
)

// newSTSClient builds an STS SDK client. Returns an error on nil opts so
// callers fail fast instead of dereferencing nil later.
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	c, err := sts.NewClient(&openapi.Config{
		AccessKeyId:     tea.String(opts.AccessKeyId),
		AccessKeySecret: tea.String(opts.AccessKeySecret),
		RegionId:        tea.String(opts.RegionId),
		Endpoint:        tea.String(opts.Endpoint),
		Protocol:        tea.String(opts.Protocol),
		ReadTimeout:     opts.ReadTimeout,
		ConnectTimeout:  opts.ConnectTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create sts client: %w", err)
	}
	return &stsClient{cli: c}, nil
}

// GetSTSToken retrieves temporary STS credentials via AssumeRole. Requires
// RoleARN to be configured at NewAliyunProvider time; otherwise returns an
// explicit error so callers know to use GenerateUploadURL instead.
//
// The Aliyun STS SDK ignores ctx (same limitation as aliyun-oss-go-sdk);
// cancellation/timeout must be configured via stsClientOpts.ConnectTimeout/ReadTimeout.
func (p *AliyunProvider) GetSTSToken(_ context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil || p.roleARN == "" {
		return nil, fmt.Errorf("aliyun STS not configured for this provider; set provider.role_arn in config")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}

	policyJSON, err := buildAliyunPolicy(policy, p.region, parseAccountUID(p.roleARN))
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int64(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	// Aliyun AssumeRole rejects DurationSeconds < 900 with an opaque API error.
	// Fail fast here so callers get an actionable message instead of a wrapped
	// SDK error from the cloud. Upper bound is not checked — the role's
	// MaxSessionDuration (default 3600s) governs that and varies per role.
	if duration < minAliyunSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below Aliyun AssumeRole minimum of %ds",
			policy.TTL, minAliyunSTSDuration)
	}

	// RoleSessionName embeds OwnerID so OSS audit logs can trace credentials
	// back to the originating user. OwnerID is not sensitive.
	resp, err := p.stsCli.assumeRole(&assumeRoleReq{
		RoleArn:         p.roleARN,
		RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
		DurationSeconds: &duration,
		Policy:          policyJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("aliyun sts assume role: %w", err)
	}

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

// assumeRole calls STS AssumeRole and maps the response to project types.
// A nil Policy is omitted so the role's full permissions apply.
func (c *stsClient) assumeRole(req *assumeRoleReq) (*assumeRoleResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil assume role req")
	}
	r := &sts.AssumeRoleRequest{
		RoleArn:         tea.String(req.RoleArn),
		DurationSeconds: req.DurationSeconds,
		RoleSessionName: tea.String(req.RoleSessionName),
	}
	if req.Policy != nil {
		policyBytes, err := marshalPolicyJSON(req.Policy)
		if err != nil {
			return nil, fmt.Errorf("marshal policy: %w", err)
		}
		r.Policy = tea.String(string(policyBytes))
	}
	resp, err := c.cli.AssumeRole(r)
	if err != nil {
		return nil, fmt.Errorf("assume role: %w", err)
	}
	if resp == nil || resp.Body == nil || resp.Body.Credentials == nil {
		return nil, fmt.Errorf("assume role returned empty credentials")
	}
	return &assumeRoleResp{
		RequestId:       tea.StringValue(resp.Body.RequestId),
		AccessKeyId:     tea.StringValue(resp.Body.Credentials.AccessKeyId),
		AccessKeySecret: tea.StringValue(resp.Body.Credentials.AccessKeySecret),
		SecurityToken:   tea.StringValue(resp.Body.Credentials.SecurityToken),
		Expiration:      tea.StringValue(resp.Body.Credentials.Expiration),
	}, nil
}

// --- internal helpers ---

// buildAliyunPolicy translates STSPolicy into the JSON structure expected by
// Aliyun AssumeRole's Policy parameter. Returns map[string]any so the
// stsClient can marshal it with HTML escaping disabled (Aliyun rejects
// <-encoded JSON).
//
// Translation rules:
//   - Bucket + KeyPrefix → Resource prefix "acs:oss:<region>:<accountUID>:<bucket>/<prefix>/*"
//     region and accountUID are scoped when provided (non-empty); empty falls
//     back to "*" to preserve prior behavior. Tighten these to prevent a
//     leaked credential from being used against same-named buckets in other
//     regions or accounts.
//   - AllowedExtensions (each must start with '.') → one Resource entry per ext
//   - AllowedActions defaults to ["oss:PutObject"] for credential hardening
//   - MaxSize is intentionally NOT mapped: Aliyun PutObject has no STS-side
//     size enforcement (only PostObject content-length-range supports it).
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for oss:PutObjectAcl.
func buildAliyunPolicy(p *types.STSPolicy, region, accountUID string) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"oss:PutObject"}
	}

	// Trim trailing/leading slashes so empty or "/" KeyPrefix doesn't produce
	// double-slash resource patterns ("acs:oss:*:*:bucket//*") that Aliyun RAM
	// matches literally and silently rejects at PUT time.
	prefix := strings.Trim(p.KeyPrefix, "/")
	scopedResource := fmt.Sprintf("acs:oss:%s:%s:%s", orWildcard(region), orWildcard(accountUID), p.Bucket)
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
		"Effect":   "Allow",
		"Action":   actions,
		"Resource": resources,
	}

	// Condition is only added when at least one condition is enabled — Aliyun
	// RAM rejects an empty Condition block, so we omit it entirely when no
	// hardening is requested.
	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string]string{"acs:SecureTransport": "true"}
	}
	if p.LockObjectACL {
		conditions["StringEquals"] = map[string]string{"oss:x-oss-object-acl": "private"}
	}
	if len(conditions) > 0 {
		allowStmt["Condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	// Explicit Deny on PutObjectAcl prevents clients from changing the ACL of
	// an uploaded object to public-read. Resource matches the same scoped set
	// as the Allow statement so the deny applies exactly to what the credential
	// could upload.
	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"Effect":   "Deny",
			"Action":   []string{"oss:PutObjectAcl"},
			"Resource": resources,
		})
	}

	return map[string]any{
		"Version":   "1",
		"Statement": statements,
	}, nil
}

// stsEndpointFor maps an Aliyun region ID to its STS regional endpoint. Empty
// region falls back to cn-hangzhou (the most common setup). Callers should log
// if they hit the fallback — it usually means misconfiguration.
func stsEndpointFor(region string) string {
	if region == "" {
		return "sts.cn-hangzhou.aliyuncs.com"
	}
	return fmt.Sprintf("sts.%s.aliyuncs.com", region)
}

// orWildcard returns s when non-empty, otherwise "*" — used to keep Resource
// ARN segments permissive when caller did not supply a scope. Tighten callers
// by passing region and accountUID extracted from roleARN.
func orWildcard(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// parseAccountUID extracts the Alibaba Cloud account UID from a RAM role ARN.
// roleARN format: acs:ram::<account_uid>:role/<role_name>. Returns empty
// string on malformed input so the caller falls back to wildcard Resource.
func parseAccountUID(roleARN string) string {
	if roleARN == "" {
		return ""
	}
	parts := strings.Split(roleARN, ":")
	// Expected layout: ["acs", "ram", "", "<account_uid>", "role/<name>"] OR
	// with an extra empty segment depending on whether the UID is present.
	// Validate prefix to avoid returning nonsense from non-RAM ARNs.
	if len(parts) < 5 || parts[0] != "acs" || parts[1] != "ram" {
		return ""
	}
	return parts[3]
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled.
// Aliyun policy JSON must not escape `<`, `>`, or `&` or AssumeRole rejects it
// as malformed. sonic (underlying jsonx) does not escape HTML by default,
// so this is a plain Marshal — no encoder setup or newline trimming needed.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	return jsonx.Marshal(p)
}
