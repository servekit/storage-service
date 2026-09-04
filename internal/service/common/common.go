// Package common holds helpers shared across service subpackages
// (audit, quota, upload, etc.) to prevent duplication.
package common

import (
	"context"

	gidv1 "github.com/servekit/gid-service/gen/gid/v1"
	gidservice "github.com/servekit/gid-service/pkg"
)

// NextID fetches one int64 ID from the gid dependency over the generated
// (proto-shaped) interface, unwrapping the request/response for callers that
// just need the number.
func NextID(ctx context.Context, gid gidservice.Service) (int64, error) {
	resp, err := gid.NextID(ctx, &gidv1.NextIDRequest{})
	if err != nil {
		return 0, err
	}
	return resp.GetId(), nil
}
