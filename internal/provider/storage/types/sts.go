package types

import "time"

// STSPolicy defines the access policy for an STS (Security Token Service)
// credential. It describes the scope a cloud provider should honor when minting
// short-lived credentials, independent of any caching/business semantics.
type STSPolicy struct {
	// OwnerID + OwnerType identify the requester. Providers may embed OwnerID in
	// RoleSessionName (Aliyun) so OSS audit logs can trace a credential back to
	// the originating user.
	OwnerID   int64
	OwnerType int32

	Bucket    string
	KeyPrefix string

	// AllowedExtensions restricts which file suffixes the credential may PUT.
	// Each element MUST start with '.' (e.g. ".jpg") and SHOULD be pre-normalized
	// to lowercase by the caller. Translated to Resource wildcards on Aliyun,
	// enforced by OSS at PUT time. Empty = no suffix restriction.
	AllowedExtensions []string

	// AllowedActions restricts which OSS actions the credential may perform.
	// Defaults to ["oss:PutObject"] in AliyunProvider if empty — credentials
	// are minted for uploads, not general bucket access.
	AllowedActions []string

	// MaxSize is NOT enforced by Aliyun OSS PutObject (only PostObject supports
	// content-length-range). Kept as a service-layer soft limit; cloud-side
	// enforcement requires bucket policy configuration out of band.
	MaxSize int64

	// EnforceHTTPS adds Condition {"Bool": {"acs:SecureTransport": "true"}} to
	// the Allow statement, blocking plaintext HTTP uploads at OSS. Default false
	// to preserve backward compatibility with existing callers.
	EnforceHTTPS bool

	// LockObjectACL adds Condition {"StringEquals": {"oss:x-oss-object-acl":
	// "private"}}, forcing uploaded objects to private regardless of
	// client-sent x-oss-object-acl headers. Pair with DenyPutObjectACL for
	// defense in depth.
	LockObjectACL bool

	// DenyPutObjectACL emits a separate Deny statement for oss:PutObjectAcl,
	// preventing clients from changing an object's ACL to public-read after
	// the initial PUT. Without this, the Allow-only Policy still lets
	// clients call PutObjectAcl on the uploaded key.
	DenyPutObjectACL bool

	TTL time.Duration
}

// STSCredential holds temporary STS credentials returned by a cloud provider.
// These are raw cloud data shapes (no cache key, TTL policy, or singleflight);
// business semantics live in internal/service/sts.
type STSCredential struct {
	AccessKey       string
	SecretKey       string
	SecurityToken   string
	Endpoint        string
	Region          string
	Bucket          string
	ObjectKeyPrefix string
	ExpiresAt       time.Time
}
