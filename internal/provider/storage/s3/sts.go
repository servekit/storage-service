package s3

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// stsClient wraps the AWS STS SDK so the rest of the s3 package can issue
// AssumeRole calls without exposing SDK types to callers.
type stsClient struct {
	cli *awssts.Client
}

// stsClientOpts configures newSTSClient. Endpoint empty → AWS regional STS
// (sts.<Region>.amazonaws.com); non-empty → MinIO/custom STS endpoint.
type stsClientOpts struct {
	AccessKey string
	SecretKey string
	Region    string
	Endpoint  string
}

// assumeRoleReq is the project-typed input for AssumeRole. DurationSeconds is
// int64 for cross-provider consistency with aliyun's assumeRoleReq (the AWS SDK
// field is *int32; assumeRole narrows at the SDK boundary). AWS accepts 900..43200.
type assumeRoleReq struct {
	RoleArn         string
	RoleSessionName string
	DurationSeconds *int64
	Policy          map[string]any
}

// assumeRoleResp carries the temporary credentials. Expiration is the raw
// ISO8601 string from AWS STS; callers parse it to time.Time.
type assumeRoleResp struct {
	AccessKeyId     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      string
}

// assumeRoleCaller is the contract stsClient satisfies. Tests inject a fake.
type assumeRoleCaller interface {
	assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error)
}

const (
	// minAWSSTSDuration is the lower bound AWS STS enforces on
	// DurationSeconds. Fail fast below this so callers get an actionable error
	// instead of an opaque SDK API failure.
	minAWSSTSDuration int64 = 900
)

// newSTSClient builds an AWS STS SDK client. Empty endpoint → AWS regional
// STS endpoint derived from region; non-empty → custom (MinIO/S3-compat).
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	creds := credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, "")

	var stsOpts []func(*awssts.Options)
	if opts.Endpoint != "" {
		stsOpts = append(stsOpts, func(o *awssts.Options) {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		})
	}

	cli := awssts.New(awssts.Options{
		Region:      opts.Region,
		Credentials: creds,
	}, stsOpts...)
	return &stsClient{cli: cli}, nil
}

// GetSTSToken retrieves temporary STS credentials via AssumeRole. Requires
// RoleARN to be configured at NewS3Provider time; otherwise returns an
// explicit error so callers know to use GenerateUploadURL instead.
//
// Endpoint policy: if NewS3Provider was constructed with a custom endpoint
// (MinIO/S3-compat), STS hits that same endpoint; otherwise AWS regional STS.
func (p *S3Provider) GetSTSToken(ctx context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil || p.roleARN == "" {
		return nil, fmt.Errorf("s3 STS not configured for this provider; set provider.role_arn in config")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}

	policyJSON, err := buildS3Policy(policy)
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int64(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	if duration < minAWSSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below AWS STS minimum of %ds",
			policy.TTL, minAWSSTSDuration)
	}

	// RoleSessionName embeds OwnerID so S3 audit logs can trace credentials
	// back to the originating user.
	resp, err := p.stsCli.assumeRole(ctx, &assumeRoleReq{
		RoleArn:         p.roleARN,
		RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
		DurationSeconds: &duration,
		Policy:          policyJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 sts assume role: %w", err)
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

// assumeRole calls AWS STS AssumeRole and maps the response to project types.
// A nil Policy is omitted so the role's full permissions apply.
func (c *stsClient) assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil assume role req")
	}
	in := &awssts.AssumeRoleInput{
		RoleArn:         aws.String(req.RoleArn),
		RoleSessionName: aws.String(req.RoleSessionName),
	}
	if req.DurationSeconds != nil {
		// SDK field is *int32; project struct carries *int64 (matches Aliyun).
		in.DurationSeconds = aws.Int32(int32(*req.DurationSeconds))
	}
	if req.Policy != nil {
		policyBytes, err := marshalPolicyJSON(req.Policy)
		if err != nil {
			return nil, fmt.Errorf("marshal policy: %w", err)
		}
		in.Policy = aws.String(string(policyBytes))
	}

	out, err := c.cli.AssumeRole(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("assume role: %w", err)
	}
	if out == nil || out.Credentials == nil {
		return nil, fmt.Errorf("assume role returned empty credentials")
	}
	return &assumeRoleResp{
		AccessKeyId:     aws.ToString(out.Credentials.AccessKeyId),
		AccessKeySecret: aws.ToString(out.Credentials.SecretAccessKey),
		SecurityToken:   aws.ToString(out.Credentials.SessionToken),
		Expiration:      out.Credentials.Expiration.Format("2006-01-02T15:04:05Z"),
	}, nil
}

// --- internal helpers ---

// buildS3Policy translates STSPolicy into the JSON structure expected by
// AWS STS AssumeRole's Policy parameter. Returns map[string]any so the
// stsClient can marshal it with HTML escaping disabled.
//
// Translation rules:
//   - Bucket + KeyPrefix → Resource prefix "arn:aws:s3:::<bucket>/<prefix>/*"
//     (S3 ARNs have NO region/account segment — S3 is a global service.)
//   - AllowedExtensions (each must start with '.') → one Resource entry per ext
//   - AllowedActions defaults to ["s3:PutObject"] for credential hardening
//   - MaxSize is intentionally NOT mapped: S3 PutObject has no STS-side size
//     enforcement (same as Aliyun; only PostObject supports content-length-range).
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for s3:PutObjectAcl.
func buildS3Policy(p *types.STSPolicy) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"s3:PutObject"}
	}

	prefix := strings.Trim(p.KeyPrefix, "/")
	var base string
	if prefix == "" {
		base = fmt.Sprintf("arn:aws:s3:::%s/*", p.Bucket)
	} else {
		base = fmt.Sprintf("arn:aws:s3:::%s/%s/*", p.Bucket, prefix)
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

	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string]string{"aws:SecureTransport": "true"}
	}
	if p.LockObjectACL {
		conditions["StringEquals"] = map[string]string{"s3:x-amz-acl": "private"}
	}
	if len(conditions) > 0 {
		allowStmt["Condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"Effect":   "Deny",
			"Action":   []string{"s3:PutObjectAcl"},
			"Resource": resources,
		})
	}

	return map[string]any{
		"Version":   "2012-10-17", // AWS IAM policy version
		"Statement": statements,
	}, nil
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled.
// Mirrors the Aliyun rationale: some S3-compatible backends reject HTML-
// escaped JSON. sonic (underlying jsonx) does not escape HTML by default,
// so this is a plain Marshal — no encoder setup or newline trimming needed.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	return jsonx.Marshal(p)
}
