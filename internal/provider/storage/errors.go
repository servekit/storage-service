package storage

import "github.com/servekit/storage-service/internal/provider/storage/types"

// ErrObjectNotFound is re-exported from the types/ subpackage. The canonical
// definition lives there so vendor-specific subpackages (e.g. aliyun/) can
// return the same sentinel value without an import cycle.
//
// See types.ErrObjectNotFound for the full documentation.
var ErrObjectNotFound = types.ErrObjectNotFound
