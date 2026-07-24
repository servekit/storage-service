package huawei

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/global"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/config"
	iam "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3/model"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3/region"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// stsClient wraps the Huawei IAM SDK so the rest of the huawei package can
// issue CreateTemporaryAccessKeyByAgency calls without exposing SDK types.
//
// Huawei splits "STS" across two unrelated services:
//   - OBS has its own PostObject policy mechanism (not used here).
//   - IAM has CreateTemporaryAccessKeyByAgency (THIS one) — issues temp
//     AK/SK bound to a delegated agency (委托) for cross-account or scoped
//     access. We use IAM because the policy syntax is richer and matches
//     the design spec.
//
// SDK request shape (verified against huaweicloud-sdk-go-v3 v0.1.202):
//
//	CreateTemporaryAccessKeyByAgencyRequest.Body.Auth
//	  -> AgencyAuth.Identity (AgencyAuthIdentity)
//	       .Methods        = ["assume_role"]
//	       .AssumeRole     = IdentityAssumerole{
//	                           AgencyName:      "<agency name>",
//	                           DomainId:        ptr("<account UID>"),  // one of DomainId/DomainName required; we use DomainId
//	                           DurationSeconds: ptr(int32),
//	                           SessionUser:     &AssumeroleSessionuser{Name: ptr("<session-name>")},
//	                         }
//	       .Policy         = ServicePolicy{
//	                           Version: "1.1",
//	                           Statement: []ServiceStatement{...},  // typed, NOT a JSON string body
//	                         }
//
// Note: ctx is accepted to satisfy the assumeAgencyCaller contract but the
// IAM SDK's CreateTemporaryAccessKeyByAgency (this version) does not accept a
// context argument; the Invoker variant exists but does not expose
// WithContext in v0.1.202. The deadline is therefore advisory only — caller
// timeouts must be enforced at the HTTP client level if needed.
type stsClient struct {
	cli *iam.IamClient
}

// stsClientOpts configures newSTSClient.
type stsClientOpts struct {
	AccessKey string
	SecretKey string
	// DomainID is the Huawei account ID. Required by IAM's global
	// credentials builder; the agency call is a global-scope API.
	DomainID string
	Region   string
	// Endpoint overrides the IAM regional endpoint. Empty falls back to
	// the SDK's region-derived endpoint. Tests pass the httptest.Server
	// host here.
	Endpoint string
}

// assumeAgencyReq is the project-typed input for
// CreateTemporaryAccessKeyByAgency. DurationSeconds matches the SDK field
// type (int32; Huawei accepts 900..43200).
type assumeAgencyReq struct {
	AgencyName      string
	DomainID        string
	RoleSessionName string
	DurationSeconds int32
	Policy          map[string]any
}

// assumeAgencyResp carries the temporary credentials. ExpiresAt is parsed
// from the IAM "expires_at" RFC3339 string; callers consume time.Time.
type assumeAgencyResp struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	ExpiresAt     time.Time
}

// assumeAgencyCaller is the contract stsClient satisfies. Defining it as
// an interface lets tests inject a fake without exposing the SDK wrapper.
type assumeAgencyCaller interface {
	assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error)
}

const (
	// minHuaweiSTSDuration is the lower bound Huawei IAM enforces on
	// DurationSeconds (900s = 15min). We fail fast below this so callers
	// get an actionable error instead of a wrapped SDK API failure.
	minHuaweiSTSDuration int32 = 900
	// maxHuaweiSTSDuration is the upper bound (43200s = 12h). Failures
	// above this are surfaced as a clear local error rather than an IAM
	// rejection.
	maxHuaweiSTSDuration int32 = 43200
)

// newSTSClient builds an IAM SDK client with global-scope credentials.
// Returns an error on nil opts or empty required fields so callers fail
// fast instead of dereferencing nil later.
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("access_key and secret_key required")
	}
	if opts.DomainID == "" {
		return nil, fmt.Errorf("domain_id required (Huawei account UID)")
	}

	auth, err := global.NewCredentialsBuilder().
		WithAk(opts.AccessKey).
		WithSk(opts.SecretKey).
		WithDomainId(opts.DomainID).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("build iam credentials: %w", err)
	}

	builder := iam.IamClientBuilder().
		WithCredential(auth).
		WithHttpConfig(config.DefaultHttpConfig())
	if opts.Endpoint != "" {
		// Custom endpoint (tests). The SDK's region value is not
		// consulted when endpoint is set on the HTTP config.
		builder = builder.WithEndpoint(opts.Endpoint)
	} else if opts.Region != "" {
		// Map region string to SDK region value (e.g. "cn-north-4" →
		// region.CN_NORTH_4). Verified: cn-north-4 IS pre-registered in
		// the IAM SDK's region map (region.go:13). region.ValueOf panics
		// on empty input and returns *Region (no error) on miss; a nil
		// result means the region is unknown and we fall back to the
		// endpoint derived by iamEndpointFor at the caller side.
		if r := region.ValueOf(opts.Region); r != nil {
			builder = builder.WithRegion(r)
		}
	}
	hcClient, err := builder.SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("build iam http client: %w", err)
	}
	return &stsClient{cli: iam.NewIamClient(hcClient)}, nil
}

// assumeAgency calls IAM CreateTemporaryAccessKeyByAgency and maps the
// response to project types. ctx is currently advisory (see stsClient doc
// comment): the SDK's direct method does not accept a context in
// v0.1.202. Callers needing hard timeouts must configure the HTTP client.
func (c *stsClient) assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error) {
	_ = ctx
	if req == nil {
		return nil, fmt.Errorf("nil assume agency req")
	}
	if req.Policy == nil {
		return nil, fmt.Errorf("nil policy (Huawei requires a policy on agency temp credentials)")
	}
	// Enforce Huawei IAM's DurationSeconds bounds locally. The constants
	// below are the documented IAM limits (900s..43200s); failing fast here
	// gives callers an actionable message instead of a wrapped SDK failure
	// from the IAM backend.
	if req.DurationSeconds < minHuaweiSTSDuration {
		return nil, fmt.Errorf("huawei sts: duration %ds below min %ds", req.DurationSeconds, minHuaweiSTSDuration)
	}
	if req.DurationSeconds > maxHuaweiSTSDuration {
		return nil, fmt.Errorf("huawei sts: duration %ds above max %ds", req.DurationSeconds, maxHuaweiSTSDuration)
	}

	// Translate the loose map[string]any policy into the SDK's typed
	// ServicePolicy / ServiceStatement. We build typed statements instead
	// of stuffing a JSON string into a "Body" field because the IAM SDK
	// v0.1.202 model.ServicePolicy has NO Body field — only Version +
	// []ServiceStatement. Verified in
	// services/iam/v3/model/model_service_policy.go.
	typedPolicy, err := buildServicePolicy(req.Policy)
	if err != nil {
		return nil, fmt.Errorf("translate policy: %w", err)
	}

	duration := req.DurationSeconds
	sessionName := req.RoleSessionName
	domainID := req.DomainID
	sdkReq := &model.CreateTemporaryAccessKeyByAgencyRequest{
		Body: &model.CreateTemporaryAccessKeyByAgencyRequestBody{
			Auth: &model.AgencyAuth{
				Identity: &model.AgencyAuthIdentity{
					Methods: []model.AgencyAuthIdentityMethods{
						model.GetAgencyAuthIdentityMethodsEnum().ASSUME_ROLE,
					},
					AssumeRole: &model.IdentityAssumerole{
						AgencyName:      req.AgencyName,
						DomainId:        &domainID,
						DurationSeconds: &duration,
						SessionUser: &model.AssumeroleSessionuser{
							Name: &sessionName,
						},
					},
					Policy: typedPolicy,
				},
			},
		},
	}

	resp, err := c.cli.CreateTemporaryAccessKeyByAgency(sdkReq)
	if err != nil {
		return nil, fmt.Errorf("create temp access key by agency: %w", err)
	}
	if resp == nil || resp.Credential == nil {
		return nil, fmt.Errorf("agency credential response was empty")
	}
	cred := resp.Credential

	expiresAt, perr := parseHuaweiExpiry(cred.ExpiresAt)
	if perr != nil {
		return nil, fmt.Errorf("parse agency credential expires_at %q: %w", cred.ExpiresAt, perr)
	}

	return &assumeAgencyResp{
		AccessKey:     cred.Access,
		SecretKey:     cred.Secret,
		SecurityToken: cred.Securitytoken,
		ExpiresAt:     expiresAt,
	}, nil
}

// buildServicePolicy converts the loose map[string]any policy produced by
// buildHuaweiPolicy into the SDK's typed *model.ServicePolicy. The SDK
// expects typed statements (Effect is an enum, Condition is a triple-nested
// map), so we round-trip via JSON to avoid duplicating the validation logic
// in two shapes.
func buildServicePolicy(p map[string]any) (*model.ServicePolicy, error) {
	raw, err := jsonx.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal policy map: %w", err)
	}
	var typed model.ServicePolicy
	if err := jsonx.Unmarshal(raw, &typed); err != nil {
		return nil, fmt.Errorf("unmarshal into service policy: %w", err)
	}
	if typed.Version == "" {
		return nil, fmt.Errorf("policy missing Version")
	}
	if len(typed.Statement) == 0 {
		return nil, fmt.Errorf("policy missing Statement")
	}
	return &typed, nil
}

// --- internal helpers ---

// buildHuaweiPolicy translates STSPolicy into the JSON structure expected
// by Huawei IAM's CreateTemporaryAccessKeyByAgency Policy field.
//
// Translation rules (vendor-specific; Huawei's IAM policy syntax is
// documented at https://support.huaweicloud.com/usermanual-iam/iam_01_001.html
// and differs from AWS/Aliyun RAM in resource ARN format):
//   - Version is always "1.1" (Huawei's current policy version).
//   - Bucket + KeyPrefix → Resource "OBS:*:*:object:<bucket>/<prefix>/*"
//     where the first "*" is the region slot and the second "*" is the
//     account-UID slot (Huawei's canonical 5-segment OBS resource ARN;
//     the leading "object:" type prefix is mandatory). Empty KeyPrefix or
//     "/" yields "OBS:*:*:object:<bucket>/*" (no double slash).
//   - AllowedExtensions (each must start with '.') → one Resource entry
//     per ext.
//   - AllowedActions defaults to ["obs:object:PutObject"] for credential
//     hardening. Huawei action names use the "obs:object:<Action>" or
//     "obs:bucket:<Action>" pattern; "obs:object:" matches the resource
//     prefix emitted above.
//   - MaxSize is intentionally NOT mapped: Huawei OBS PutObject has no
//     STS-side size enforcement.
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for
//     "obs:object:PutObjectAcl".
//
// The output map is JSON-marshaled to fit the SDK's typed ServicePolicy
// shape via buildServicePolicy. The Condition value is the SDK's
// map[string]map[string][]string (operator -> key -> values); our builder
// emits single-element slices so the JSON unmarshal round-trip is exact.
func buildHuaweiPolicy(p *types.STSPolicy) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"obs:object:PutObject"}
	}

	// Trim trailing/leading slashes so empty or "/" KeyPrefix doesn't
	// produce double-slash resource patterns
	// ("OBS:*:*:object:bucket//*") that Huawei IAM matches literally and
	// silently rejects at PUT time.
	prefix := strings.Trim(p.KeyPrefix, "/")
	scopedBase := fmt.Sprintf("OBS:*:*:object:%s", p.Bucket)
	var base string
	if prefix == "" {
		base = scopedBase + "/*"
	} else {
		base = fmt.Sprintf("%s/%s/*", scopedBase, prefix)
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

	// Condition is only added when at least one condition is enabled —
	// Huawei IAM rejects an empty Condition block, so we omit it entirely
	// when no hardening is requested. Condition operator names are
	// Huawei-specific ("SecureTransport" instead of Aliyun's
	// "acs:SecureTransport"; "obs:objectAcl" condition key). The SDK
	// field is map[string]map[string][]string (operator -> key -> values),
	// so we wrap each value in a single-element slice.
	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string][]string{"SecureTransport": {"true"}}
	}
	if p.LockObjectACL {
		conditions["StringEquals"] = map[string][]string{"obs:objectAcl": {"private"}}
	}
	if len(conditions) > 0 {
		allowStmt["Condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	// Explicit Deny on obs:object:PutObjectAcl prevents clients from
	// changing the ACL of an uploaded object to public-read. Resource
	// matches the same scoped set as the Allow statement.
	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"Effect":   "Deny",
			"Action":   []string{"obs:object:PutObjectAcl"},
			"Resource": resources,
		})
	}

	return map[string]any{
		"Version":   "1.1",
		"Statement": statements,
	}, nil
}

// iamEndpointFor maps a Huawei region ID to its IAM regional endpoint.
// Empty region falls back to cn-north-4 (the IAM global region). Callers
// should log if they hit the fallback — it usually means misconfiguration.
func iamEndpointFor(region string) string {
	if region == "" {
		return "iam.cn-north-4.myhuaweicloud.com"
	}
	return fmt.Sprintf("iam.%s.myhuaweicloud.com", region)
}

// marshalPolicyJSON marshals the policy map deterministically. Tests use it
// to feed assert.JSONEq; the production path round-trips the same map into
// model.ServicePolicy via buildServicePolicy. Kept as a small helper so
// the JSON shape (key order, escaping) is consistent between the two
// consumers.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	return jsonx.Marshal(p)
}

// parseHuaweiExpiry parses an IAM "expires_at" RFC3339 timestamp. Returns
// an error rather than silently zeroing so callers can detect malformed
// cloud responses instead of trusting a zero time.
func parseHuaweiExpiry(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty expires_at")
	}
	// IAM returns RFC3339 (e.g. "2026-06-26T15:30:00.000000Z"). Try the
	// canonical layout first, then the no-zone layout as a fallback.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized expires_at format")
}
