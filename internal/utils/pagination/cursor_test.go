package pagination

import (
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []PageCursor{
		{ID: 42},
		{ID: 42, Filename: "yankee"},
		{ID: 42, Filename: "yankee", CreatedAt: "2026-06-23T10:00:00Z"},
		{ID: 42, CreatedAt: "2026-06-23T10:00:00Z"},
	}
	for _, c := range cases {
		tok := EncodePageCursor(c)
		got, err := DecodePageCursor(tok)
		if err != nil {
			t.Fatalf("decode %q: %v", tok, err)
		}
		if got != c {
			t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, c)
		}
	}
}

func TestDecodeLegacyBareNumeric(t *testing.T) {
	got, err := DecodePageCursor("12345")
	if err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if got.ID != 12345 || got.Filename != "" || got.CreatedAt != "" {
		t.Fatalf("legacy decode mismatch: got=%+v", got)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	cases := []string{"", "garbage", "v1.garbage!!", "v1."}
	for _, tok := range cases {
		if _, err := DecodePageCursor(tok); err == nil {
			t.Fatalf("expected error for %q", tok)
		}
	}
}

func TestCursorFromCreatedAtRoundTrip(t *testing.T) {
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	s := CursorFromCreatedAt(now)
	got := CursorToCreatedAt(s)
	if !got.Equal(now) {
		t.Fatalf("round-trip mismatch: got=%v want=%v", got, now)
	}

	if CursorFromCreatedAt(time.Time{}) != "" {
		t.Fatalf("zero time should encode as empty string")
	}
	if !CursorToCreatedAt("").IsZero() {
		t.Fatalf("empty string should decode as zero time")
	}
	if !CursorToCreatedAt("not-a-time").IsZero() {
		t.Fatalf("garbage should decode as zero time")
	}
}
