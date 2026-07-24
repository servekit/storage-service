package types

import "errors"

// ErrObjectNotFound is the sentinel error returned by Provider operations
// (HeadObject, GetObject, etc.) when the requested object does not exist in the
// underlying cloud store. Callers that need to distinguish "object genuinely
// absent" from transient failures (network blip, OSS 5xx, timeout) must wrap
// provider errors and check with errors.Is(err, ErrObjectNotFound).
//
// Each concrete Provider translates its SDK-specific "not found" signal into
// this sentinel:
//   - AliyunProvider: oss.ServiceError with StatusCode 404 ("NoSuchKey").
//   - S3Provider:     *s3types.NoSuchKey / *s3types.NotFound (HTTP 404).
//   - FakeProvider:   returns it directly when the key is not in the map.
//
// It lives in the types/ leaf package so that vendor-specific subpackages
// (e.g. aliyun/) can return it without importing the parent storage package.
var ErrObjectNotFound = errors.New("provider: object not found")
