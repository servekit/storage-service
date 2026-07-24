// Package fake provides an in-memory types.Provider used by service-layer
// integration tests. It supports PutObject / GetObject / DeleteObject /
// HeadObject against a per-instance map keyed by (bucket, key), plus
// pass-through Presign* and STS stubs that tests rarely need to assert on.
//
// Tests inject a *FakeProvider via storage.NewRegistryWithProvider (the
// helper itself lives in the parent storage package because it needs access
// to Registry's unexported fields).
package fake

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// FakeProvider is an in-memory Provider used by service-layer integration
// tests.
type FakeProvider struct {
	mu       sync.Mutex
	objects  map[string]*fakeObject // key: bucket+"/"+key
	headErrs map[string]error       // key: bucket+"/"+key → pinned HeadObject error
	sts      *types.STSCredential
	stsCalls int // incremented each time GetSTSToken is invoked
}

type fakeObject struct {
	data        []byte
	contentType string
	etag        string
	objectACL   string
	modtime     time.Time
}

// Compile-time assertion that *FakeProvider satisfies types.Provider.
var _ types.Provider = (*FakeProvider)(nil)

// NewFakeProvider returns an empty FakeProvider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		objects:  make(map[string]*fakeObject),
		headErrs: make(map[string]error),
	}
}

// SetSTSCredential configures the credential returned by GetSTSToken.
func (p *FakeProvider) SetSTSCredential(c *types.STSCredential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sts = c
}

// PutObject stores the given bytes under (bucket, key).
func (p *FakeProvider) PutObject(_ context.Context, bucket, key string, reader io.Reader, _ int64, opts ...types.PutOption) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	po := types.NewPutOptions(opts...)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[bucket+"/"+key] = &fakeObject{
		data:        body,
		contentType: po.ContentType,
		modtime:     time.Now().UTC(),
	}
	return nil
}

// PutObjectWithMD5 stores bytes under (bucket, key) and pins the ETag to the
// supplied md5Hex (quoted, matching S3 conventions). Tests use this to simulate
// a client-declared MD5 without having to fabricate bytes that hash to it.
func (p *FakeProvider) PutObjectWithMD5(_ context.Context, bucket, key string, data []byte, contentType, md5Hex string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[bucket+"/"+key] = &fakeObject{
		data:        data,
		contentType: contentType,
		etag:        `"` + md5Hex + `"`,
		modtime:     time.Now().UTC(),
	}
}

// SetObjectACL pins the per-object ACL that HeadObject will report for
// (bucket, key). Tests use this to simulate a cloud that returned
// "public-read" on a misconfigured upload, exercising the service-layer
// ACL-verification path without an integration test.
func (p *FakeProvider) SetObjectACL(bucket, key, acl string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	obj, ok := p.objects[bucket+"/"+key]
	if !ok {
		// Create a placeholder so HeadObject can report the ACL even when no
		// bytes were ever PUT. Tests asserting both presence and ACL usually
		// call PutObject first; this branch covers narrow ACL-only assertions.
		obj = &fakeObject{modtime: time.Now().UTC()}
		p.objects[bucket+"/"+key] = obj
	}
	obj.objectACL = acl
}

// GetObject returns a reader over the stored bytes for (bucket, key).
func (p *FakeProvider) GetObject(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	obj, ok := p.objects[bucket+"/"+key]
	if !ok {
		return nil, types.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.data)), nil
}

// DeleteObject removes the stored bytes for (bucket, key).
func (p *FakeProvider) DeleteObject(_ context.Context, bucket, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.objects, bucket+"/"+key)
	return nil
}

// ObjectExists reports whether an object is stored under (bucket, key).
// Test helper for asserting presence/absence after operations like GC.
func (p *FakeProvider) ObjectExists(bucket, key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.objects[bucket+"/"+key]
	return ok
}

// HeadObject returns metadata for (bucket, key). If a transient error has been
// pinned for (bucket, key) via SetHeadObjectError, that error is returned
// verbatim so callers can exercise retry behavior. When the object is absent,
// it returns types.ErrObjectNotFound (matching the convention documented on
// the sentinel).
func (p *FakeProvider) HeadObject(_ context.Context, bucket, key string) (*types.ObjectInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.headErrs[bucket+"/"+key]; ok {
		return nil, err
	}
	obj, ok := p.objects[bucket+"/"+key]
	if !ok {
		return nil, types.ErrObjectNotFound
	}
	etag := obj.etag
	if etag == "" {
		etag = `"` + md5Hex(obj.data) + `"`
	}
	return &types.ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		ETag:         etag,
		ContentType:  obj.contentType,
		LastModified: obj.modtime,
		ObjectACL:    obj.objectACL,
	}, nil
}

// SetHeadObjectError pins a transient error for (bucket, key) so the next
// HeadObject call returns it instead of looking the object up. Tests use this
// to simulate transient OSS failures (timeout, 5xx) and assert that callers
// retry instead of treating the object as absent. Pass nil to clear the pin.
func (p *FakeProvider) SetHeadObjectError(bucket, key string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.headErrs, bucket+"/"+key)
		return
	}
	p.headErrs[bucket+"/"+key] = err
}

// PresignPutObject returns a placeholder URL; tests that exercise the actual
// upload use PutObject directly. Options are accepted for interface conformance
// but do not affect the returned URL — tests needing option-aware behavior
// should hit a real provider.
func (*FakeProvider) PresignPutObject(_ context.Context, bucket, key string, _ time.Duration, _ ...types.PutPresignOption) (string, http.Header, error) {
	return "https://fake.example/" + bucket + "/" + key, nil, nil
}

// PresignGetObject returns a placeholder URL. Markers are appended so service
// tests can assert option wiring without depending on a real cloud:
//   - WithPublic() → "?public=true"
//   - WithDownloadFilename(name) → "?filename=<urlencoded name>" (composed
//     with & when both are set)
//
// Other options are accepted for interface conformance but ignored.
func (*FakeProvider) PresignGetObject(_ context.Context, bucket, key string, _ time.Duration, opts ...types.GetPresignOption) (string, error) {
	o := types.NewGetPresignOptions(opts...)
	u := "https://fake.example/" + bucket + "/" + key
	q := url.Values{}
	if o.Public {
		q.Set("public", "true")
	}
	if o.Filename != "" {
		q.Set("filename", o.Filename)
	}
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	return u, nil
}

// GetSTSToken returns the previously configured credential.
func (p *FakeProvider) GetSTSToken(_ context.Context, _ *types.STSPolicy) (*types.STSCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stsCalls++
	if p.sts == nil {
		return &types.STSCredential{}, nil
	}
	return p.sts, nil
}

// STSCalls returns the number of times GetSTSToken has been invoked. Tests
// use this to assert fail-fast paths did not reach the provider.
func (p *FakeProvider) STSCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stsCalls
}

// ListObjects returns all stored keys with the given bucket+prefix.
func (p *FakeProvider) ListObjects(_ context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	infos := make([]types.ObjectInfo, 0, len(p.objects))
	for k, obj := range p.objects {
		if !bytes.HasPrefix([]byte(k), []byte(bucket+"/")) {
			continue
		}
		key := k[len(bucket)+1:]
		if prefix != "" && !bytes.HasPrefix([]byte(key), []byte(prefix)) {
			continue
		}
		etag := obj.etag
		if etag == "" {
			etag = `"` + md5Hex(obj.data) + `"`
		}
		infos = append(infos, types.ObjectInfo{
			Key: key, Size: int64(len(obj.data)), ETag: etag,
			ContentType: obj.contentType, LastModified: obj.modtime,
			ObjectACL: obj.objectACL,
		})
	}
	return infos, nil
}

// --- internal helpers ---

// md5Hex returns the lowercase hex MD5 digest of data. Used to fabricate ETags
// for the FakeProvider when none is explicitly set.
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
