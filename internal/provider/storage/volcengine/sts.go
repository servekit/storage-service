package volcengine

import (
	"context"
	"fmt"
	"strings"

	"github.com/servekit/go-common/jsonx"
	"github.com/volcengine/volcengine-go-sdk/service/sts"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// stsClient wraps the Volcengine STS SDK so the rest of the volcengine package
// can issue AssumeRole calls without exposing SDK types.
type stsClient struct {
	cli *sts.STS
}

// stsClientOpts configures newSTSClient.
type stsClientOpts struct {
	AccessKeyId     string
	AccessKeySecret string
	Region          string
	// Endpoint overrides the STS API endpoint. Empty falls back to the SDK
	// default (open.volcengineapi.com). Set to a httptest.Server host (no
	// scheme) for tests, with DisableSSL=true so the SDK issues plain HTTP
	// requests against the test server.
	Endpoint   string
	DisableSSL bool
}

// assumeRoleReq is the project-typed input for AssumeRole. DurationSeconds is
// int32 to match the SDK field type (Volcengine AssumeRole accepts
// 900..3600 as int32).
type assumeRoleReq struct {
	RoleTrn         string
	RoleSessionName string
	DurationSeconds *int32
	Policy          map[string]any
}

// assumeRoleResp carries the temporary credentials. Expiration is the raw
// ISO8601 string from Volcengine; callers parse it to time.Time so this
// package stays free of time-zone assumptions.
type assumeRoleResp struct {
	ResponseId      string
	AccessKeyId     string
	AccessKeySecret string
	SessionToken    string
	Expiration      string
}

// assumeRoleCaller is the contract stsClient satisfies. Defining it as an
// interface lets tests inject a fake without exposing the SDK wrapper type.
type assumeRoleCaller interface {
	assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error)
}

const (
	// minVolcSTSDuration is the lower bound Volcengine AssumeRole enforces on
	// DurationSeconds. Fail fast below this so callers get an actionable error
	// instead of a wrapped SDK API failure.
	minVolcSTSDuration int32 = 900
)

// newSTSClient builds a Volcengine STS SDK client. Returns an error on nil
// opts so callers fail fast instead of dereferencing nil later.
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	cfg := volcengine.NewConfig().
		WithRegion(opts.Region).
		WithCredentials(credentials.NewStaticCredentials(opts.AccessKeyId, opts.AccessKeySecret, ""))
	if opts.Endpoint != "" {
		cfg = cfg.WithEndpoint(opts.Endpoint)
	}
	if opts.DisableSSL {
		cfg = cfg.WithDisableSSL(true)
	}
	sess, err := session.NewSession(cfg)
	if err != nil {
		return nil, fmt.Errorf("create sts session: %w", err)
	}
	return &stsClient{cli: sts.New(sess)}, nil
}

// assumeRole calls Volcengine STS AssumeRole and maps the response to project
// types. A nil Policy is omitted so the role's full permissions apply. ctx is
// forwarded via AssumeRoleWithContext so cancellation / deadlines propagate.
func (c *stsClient) assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil assume role req")
	}
	input := &sts.AssumeRoleInput{
		RoleTrn:         volcengine.String(req.RoleTrn),
		RoleSessionName: volcengine.String(req.RoleSessionName),
	}
	if req.DurationSeconds != nil {
		input.DurationSeconds = volcengine.Int32(*req.DurationSeconds)
	}
	if req.Policy != nil {
		policyBytes, err := marshalPolicyJSON(req.Policy)
		if err != nil {
			return nil, fmt.Errorf("marshal policy: %w", err)
		}
		input.Policy = volcengine.String(string(policyBytes))
	}
	resp, err := c.cli.AssumeRoleWithContext(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("assume role: %w", err)
	}
	if resp == nil || resp.Credentials == nil {
		return nil, fmt.Errorf("assume role returned empty credentials")
	}
	respId := ""
	if resp.Metadata != nil {
		respId = resp.Metadata.RequestId
	}
	return &assumeRoleResp{
		ResponseId:      respId,
		AccessKeyId:     volcengine.StringValue(resp.Credentials.AccessKeyId),
		AccessKeySecret: volcengine.StringValue(resp.Credentials.SecretAccessKey),
		SessionToken:    volcengine.StringValue(resp.Credentials.SessionToken),
		Expiration:      volcengine.StringValue(resp.Credentials.ExpiredTime),
	}, nil
}

// --- policy builder ---

// buildVolcPolicy translates STSPolicy into the JSON structure expected by
// Volcengine AssumeRole's Policy parameter. Returns map[string]any so the
// stsClient can marshal it with HTML escaping disabled.
//
// Translation rules (per Volcengine TOS docs):
//   - Bucket + KeyPrefix → Resource prefix "trn:tos::<account>:<bucket>/<prefix>/*"
//     account is parsed from roleTRN when available; empty falls back to "" to
//     match Volcengine's canonical "trn:tos:::<bucket>/..." 3-colon form.
//   - AllowedExtensions (each must start with '.') → one Resource entry per ext
//   - AllowedActions defaults to ["tos:PutObject"]
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for tos:PutObjectACL.
//
// Statement keys use TitleCase (Volcengine convention) — different from
// Tencent's lowercase / Aliyun's TitleCase-but-with-acs-prefix. Volcengine
// policies do NOT carry a top-level Version field (verified against the docs).
func buildVolcPolicy(p *types.STSPolicy, account string) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"tos:PutObject"}
	}

	prefix := strings.Trim(p.KeyPrefix, "/")
	var base string
	if prefix == "" {
		base = fmt.Sprintf("trn:tos::%s:%s/*", account, p.Bucket)
	} else {
		base = fmt.Sprintf("trn:tos::%s:%s/%s/*", account, p.Bucket, prefix)
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

	// Volcengine conditions follow AWS-style operator semantics.
	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string]string{"tos:SecureTransport": "true"}
	}
	if p.LockObjectACL {
		conditions["StringEquals"] = map[string]string{"tos:x-tos-acl": "private"}
	}
	if len(conditions) > 0 {
		allowStmt["Condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"Effect":   "Deny",
			"Action":   []string{"tos:PutObjectACL"},
			"Resource": resources,
		})
	}

	return map[string]any{
		"Statement": statements,
	}, nil
}

// parseVolcAccount extracts the account id from a Volcengine role TRN.
// TRN format: trn:iam::<account-id>:role/<role-name>. Returns empty string on
// malformed input so the caller falls back to the canonical 3-colon Resource.
func parseVolcAccount(roleTRN string) string {
	if roleTRN == "" {
		return ""
	}
	parts := strings.Split(roleTRN, ":")
	// Expected: ["trn", "iam", "", "<account>", "role/<name>"]
	if len(parts) < 5 || parts[0] != "trn" || parts[1] != "iam" {
		return ""
	}
	return parts[3]
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled.
// Volcengine policy JSON tolerates escaped characters, but disabling HTML
// escaping keeps the wire payload readable and matches Aliyun behavior.
// sonic (underlying jsonx) does not escape HTML by default, so this is a
// plain Marshal — no encoder setup or newline trimming needed.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	return jsonx.Marshal(p)
}
