package upload

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/servekit/go-common/jsonx"
)

// uploadToken contains all metadata signed into an upload_token. Vendor is
// pinned at sign time so confirm can detect registry drift (e.g. bucket
// re-assigned to a different provider mid-TTL).
//
// IsPublic is intentionally absent — it is derived from the bucket ACL at
// confirm time (via session.Bucket), so a bucket ACL change between issue
// and confirm is reflected in the persisted object. The session row carries
// the issue-time IsPublic for audit/verification, but the token itself has
// no need to.
type uploadToken struct {
	SessionID   int64             `json:"sid,omitempty"`
	OwnerID     int64             `json:"oid"`
	OwnerType   int32             `json:"ot"`
	MD5         string            `json:"md5"`
	Size        int64             `json:"sz"`
	ContentType string            `json:"ct"`
	Bucket      string            `json:"bkt"`
	Vendor      int32             `json:"vdr"`
	Filename    string            `json:"fn"`
	FilePath    string            `json:"fp"`
	Description string            `json:"desc"`
	Metadata    map[string]string `json:"meta"`
	ExpiresAt   int64             `json:"exp"`
}

type tokenError struct {
	reason  string
	cause   error
	expired bool
}

// signUploadToken signs the upload token and returns the encoded string.
// Format: base64url(hmac_sha256(json, secret)) + "." + base64url(json)
func signUploadToken(token *uploadToken, secret string) (string, error) {
	payload, err := jsonx.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("marshal token: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := mac.Sum(nil)

	encoded := base64.RawURLEncoding.EncodeToString(sig) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return encoded, nil
}

// verifyUploadToken decodes and verifies the signed token string.
// It checks HMAC signature, expiration, and that the token's OwnerID and
// OwnerType match the caller's expected values. The OwnerType check closes
// a cross-owner_type confusion hole: two owners in different owner_types
// can share the same integer OwnerID, so checking OwnerID alone lets a
// caller in owner_type=2 confirm/cancel a session minted for owner_type=1.
func verifyUploadToken(encoded, secret string, expectedOwnerID int64, expectedOwnerType int32) (*uploadToken, error) {
	dotIdx := -1
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '.' {
			dotIdx = i
			break
		}
	}
	if dotIdx < 0 {
		return nil, errTokenInvalid("missing signature separator")
	}

	sigB64 := encoded[:dotIdx]
	payloadB64 := encoded[dotIdx+1:]

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, errTokenInvalid("decode signature: %w", err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, errTokenInvalid("decode payload: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return nil, errTokenInvalid("signature mismatch")
	}

	var token uploadToken
	if err := jsonx.Unmarshal(payload, &token); err != nil {
		return nil, errTokenInvalid("unmarshal payload: %w", err)
	}

	if time.Now().Unix() > token.ExpiresAt {
		return nil, errTokenExpired()
	}

	if token.OwnerID != expectedOwnerID {
		return nil, errTokenInvalid("owner ID mismatch")
	}
	if token.OwnerType != expectedOwnerType {
		return nil, errTokenInvalid("owner type mismatch")
	}

	return &token, nil
}

// --- internal helpers ---

func errTokenInvalid(msg string, args ...any) *tokenError {
	return &tokenError{reason: "upload token invalid", cause: fmt.Errorf(msg, args...)}
}

func errTokenExpired() *tokenError {
	return &tokenError{reason: "upload token expired", expired: true}
}

func (e *tokenError) Error() string {
	if e.cause != nil {
		return e.reason + ": " + e.cause.Error()
	}
	return e.reason
}

func isUploadTokenExpired(err error) bool {
	var te *tokenError
	return errors.As(err, &te) && te.expired
}

func isUploadTokenInvalid(err error) bool {
	var te *tokenError
	return errors.As(err, &te) && !te.expired
}
