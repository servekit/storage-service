package main

import (
	"testing"

	"github.com/servekit/go-common/dbx"

	"github.com/stretchr/testify/require"
)

// TestRunMigration_Idempotent verifies a second run on an already-migrated DB
// is a no-op.
func TestRunMigration_Idempotent(t *testing.T) {
	db := dbx.SetupTestDB(t)

	require.NoError(t, runMigration(db))
	require.NoError(t, runMigration(db),
		"re-running migrate on a clean DB must not error")
}
