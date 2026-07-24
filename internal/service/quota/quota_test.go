package quota

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// availableBytes is the pure-Go core behind CheckQuota and GetMyQuota's
// "remaining quota" computation. These tests cover the arithmetic edge cases
// (clamping, large values, zero/over-use) that the DB-backed methods can only
// exercise through a Postgres testcontainer.

// TestAvailableBytes_sufficient verifies the common case: total - used is
// returned as-is when usage is below the limit.
func TestAvailableBytes_sufficient(t *testing.T) {
	assert.Equal(t, int64(400), availableBytes(1000, 600))
}

// TestAvailableBytes_exactLimit verifies that exactly-at-limit usage (total ==
// used) yields 0 available but is NOT clamped to a negative value.
func TestAvailableBytes_exactLimit(t *testing.T) {
	assert.Equal(t, int64(0), availableBytes(1000, 1000))
}

// TestAvailableBytes_unused verifies an owner with zero usage sees the full
// total as available.
func TestAvailableBytes_unused(t *testing.T) {
	assert.Equal(t, int64(1<<30), availableBytes(1<<30, 0))
}

// TestAvailableBytes_overUseClampedToZero verifies that when used exceeds total
// (e.g. an admin reduced quota below current usage), the result is clamped to
// 0 rather than surfacing a confusing negative number to callers.
func TestAvailableBytes_overUseClampedToZero(t *testing.T) {
	assert.Equal(t, int64(0), availableBytes(500, 800))
}

// TestAvailableBytes_largeValues verifies the subtraction stays correct near
// int64 scale (e.g. multi-TB quotas) without wrap-around.
func TestAvailableBytes_largeValues(t *testing.T) {
	const tb = int64(1) << 40 // 1 TiB
	total := 10 * tb
	used := 3 * tb
	assert.Equal(t, 7*tb, availableBytes(total, used))
}

// TestAvailableBytes_int64Max verifies behavior at the int64 boundary: the
// max-int64 total with zero used returns max-int64 unchanged (no overflow).
func TestAvailableBytes_int64Max(t *testing.T) {
	var maxInt64 int64 = math.MaxInt64
	assert.Equal(t, maxInt64, availableBytes(maxInt64, 0))
}

// TestAvailableBytes_zeroTotal verifies a zero-total quota with no usage still
// reports 0 available (an owner provisioned with no quota cannot upload).
func TestAvailableBytes_zeroTotal(t *testing.T) {
	assert.Equal(t, int64(0), availableBytes(0, 0))
}

// TestAvailableBytes_refundScenario verifies the negative-delta semantics
// indirectly: after a refund that drops used below total, available recovers
// the full delta worth of headroom (mirrors how AddQuota with negative delta
// increases available space).
func TestAvailableBytes_refundScenario(t *testing.T) {
	// Before refund: used 900 of 1000 -> 100 available.
	assert.Equal(t, int64(100), availableBytes(1000, 900))
	// After a 500-byte refund: used 400 of 1000 -> 600 available.
	assert.Equal(t, int64(600), availableBytes(1000, 400))
}
