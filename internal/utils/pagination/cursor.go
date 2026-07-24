// Package pagination encodes and decodes opaque page cursors.
//
// Cursor-based pagination needs to carry the last row's (sort column, id)
// tuple so non-id ORDER BY columns page without dropping or duplicating
// rows. This package keeps the on-wire format out of the service layer:
// callers build a PageCursor struct, hand it to EncodePageCursor, and get
// back a string they can put in a NextPageToken. The reverse happens on
// the next request.
//
// Token format: `v1.<base64url(json)>`. Legacy bare-numeric tokens (the
// pre-cursor format that only encoded an id) are accepted on decode for
// backwards compatibility — they map to a PageCursor with only ID set.
package pagination

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
)

// PageCursor is the in-memory shape of a page token. Only the fields
// relevant to the current ORDER BY need to be populated; the rest stay
// zero and are omitted from the encoded form.
type PageCursor struct {
	ID        int64  `json:"id"`
	Filename  string `json:"fn,omitempty"`
	CreatedAt string `json:"ca,omitempty"` // RFC3339
}

// cursorVersion is the prefix tag for the current encoding. Bumping lets
// future formats coexist with already-issued tokens.
const cursorVersion = "v1"

// EncodePageCursor serializes a cursor into an opaque token.
func EncodePageCursor(c PageCursor) string {
	payload, err := jsonx.Marshal(c)
	if err != nil {
		// PageCursor only holds primitive types; jsonx.Marshal cannot fail
		// in practice. Fall back to the smallest valid token rather than
		// surfacing an error to the caller.
		payload = []byte(`{}`)
	}
	return cursorVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
}

// DecodePageCursor parses a token produced by EncodePageCursor. Tokens in
// the legacy bare-numeric form are accepted and yield a PageCursor with
// only ID populated, so callers that upgrade mid-stream keep working.
func DecodePageCursor(token string) (PageCursor, error) {
	if token == "" {
		return PageCursor{}, fmt.Errorf("empty page token")
	}

	// Legacy form: bare snowflake id, no version prefix.
	if !strings.HasPrefix(token, cursorVersion+".") {
		if id, err := strconv.ParseInt(token, 10, 64); err == nil {
			return PageCursor{ID: id}, nil
		}
		return PageCursor{}, fmt.Errorf("malformed page token")
	}

	payload := strings.TrimPrefix(token, cursorVersion+".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return PageCursor{}, fmt.Errorf("decode page token base64: %w", err)
	}

	var c PageCursor
	if err := jsonx.Unmarshal(raw, &c); err != nil {
		return PageCursor{}, fmt.Errorf("decode page token json: %w", err)
	}
	return c, nil
}

// CursorFromCreatedAt formats a time.Time into the RFC3339 string carried
// by PageCursor.CreatedAt. Empty result for the zero time so the field
// is omitted from the encoded token.
func CursorFromCreatedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// CursorToCreatedAt reverses CursorFromCreatedAt. An empty string maps
// back to the zero time.
func CursorToCreatedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
