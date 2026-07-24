package storage

import "github.com/servekit/storage-service/internal/provider/storage/types"

// Re-exported STS contract types. Definitions live in types/ subpackage.
// See provider.go for the rationale (import cycle avoidance).
type (
	STSPolicy     = types.STSPolicy
	STSCredential = types.STSCredential
)
