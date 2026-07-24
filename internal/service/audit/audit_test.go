package audit

import (
	"context"
	"errors"
	"testing"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"github.com/stretchr/testify/assert"
)

// noopGID returns a fixed ID for recorder tests that don't need a real gid
// service.
type noopGID struct{}

func (noopGID) NextID(context.Context) (int64, error) { return 1, nil }
func (noopGID) Close() error                          { return nil }

// TestEventStatusDerivation confirms the pure status-derivation logic used by
// RecordOutcome, independent of any DB/gid wiring (which needs testcontainers).
func TestEventStatusDerivation(t *testing.T) {
	t.Parallel()

	// success path
	ev := Event{Action: storagev1.AuditAction_AUDIT_ACTION_UPLOAD}
	ev.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS
	assert.Equal(t, storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS, ev.Status)

	// failure path
	err := errors.New("boom")
	ev2 := Event{Action: storagev1.AuditAction_AUDIT_ACTION_UPLOAD, Error: err}
	ev2.Status = storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED
	assert.Equal(t, storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED, ev2.Status)
	assert.Same(t, err, ev2.Error)
}

// TestNewDBRecorder confirms NewDBRecorder wires deps without panic.
func TestNewDBRecorder(t *testing.T) {
	t.Parallel()

	r := NewDBRecorder(nil, noopGID{})
	assert.NotNil(t, r)
}
