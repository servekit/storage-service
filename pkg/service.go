package pkg

import (
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
)

// Service is how a consumer holds storage-service regardless of backend: the
// in-process *Handler (module mode) and the gRPC *Client both satisfy it. It
// embeds the generated server interface so the method set tracks the proto
// automatically — no hand-maintained method list here.
type Service interface {
	storagev1.StorageServiceServer
}
