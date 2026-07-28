package handler

import (
	"testing"

	"github.com/servekit/go-common/dbx"

	"github.com/stretchr/testify/require"
)

// TestMigrate_Idempotent verifies a second run on an already-migrated DB
// is a no-op.
func TestMigrate_Idempotent(t *testing.T) {
	db := dbx.SetupTestDB(t)

	require.NoError(t, Migrate(db))
	require.NoError(t, Migrate(db),
		"re-running migrate on a clean DB must not error")
}
