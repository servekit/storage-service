package audit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/servekit/go-common/dbx"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/dal"
)

// ListMyAuditLogs returns audit log entries scoped to the caller with cursor
// pagination. Excludes entries the caller is not authorised to see.
func (s *Service) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	filter := dal.AuditLogFilter{
		Action: int32(req.GetAction()),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	if tt := req.GetTargetType(); tt != 0 {
		filter.TargetType = int32(tt)
	}
	if req.GetStartTime() != "" {
		if t, err := time.Parse(time.RFC3339, req.GetStartTime()); err == nil {
			filter.StartTime = t
		}
	}
	if req.GetEndTime() != "" {
		if t, err := time.Parse(time.RFC3339, req.GetEndTime()); err == nil {
			filter.EndTime = t
		}
	}

	if token := req.GetPageToken(); token != "" {
		if id, err := strconv.ParseInt(token, 10, 64); err == nil {
			filter.AfterID = id
		}
	}

	logs, total, err := dal.ListAuditLogsByOwner(ctx, s.db, ownerType, ownerID, filter)
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}

	pg := filter.Normalize()

	entries := make([]*storagev1.AuditLogEntry, 0, len(logs))
	for i := range logs {
		entries = append(entries, buildAuditLogEntry(&logs[i]))
	}

	entries, hasNext := dbx.TrimPage(entries, pg.PageSize)

	var nextPageToken string
	if hasNext {
		nextPageToken = fmt.Sprintf("%d", logs[pg.PageSize-1].ID)
	}

	return &storagev1.ListMyAuditLogsResponse{
		Logs:          entries,
		TotalCount:    int32(total),
		NextPageToken: nextPageToken,
	}, nil
}

// AdminListAuditLogs returns audit log entries across all owners (admin
// view) with full filter set and cursor pagination.
func (s *Service) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	filter := dal.AuditLogFilter{
		OwnerType: int32(req.GetOwnerType()),
		OwnerID:   req.GetOwnerId(),
		TargetID:  req.GetTargetId(),
		Action:    int32(req.GetAction()),
		Status:    int32(req.GetStatus()),
		RequestID: req.GetRequestId(),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	if tt := req.GetTargetType(); tt != 0 {
		filter.TargetType = int32(tt)
	}
	if req.GetStartTime() != "" {
		if t, err := time.Parse(time.RFC3339, req.GetStartTime()); err == nil {
			filter.StartTime = t
		}
	}
	if req.GetEndTime() != "" {
		if t, err := time.Parse(time.RFC3339, req.GetEndTime()); err == nil {
			filter.EndTime = t
		}
	}

	if token := req.GetPageToken(); token != "" {
		if id, err := strconv.ParseInt(token, 10, 64); err == nil {
			filter.AfterID = id
		}
	}

	logs, total, err := dal.ListAllAuditLogs(ctx, s.db, filter)
	if err != nil {
		return nil, fmt.Errorf("admin list audit logs: %w", err)
	}

	pg := filter.Normalize()

	entries := make([]*storagev1.AuditLogEntry, 0, len(logs))
	for i := range logs {
		entries = append(entries, buildAuditLogEntry(&logs[i]))
	}

	entries, hasNext := dbx.TrimPage(entries, pg.PageSize)

	var nextPageToken string
	if hasNext {
		nextPageToken = fmt.Sprintf("%d", logs[pg.PageSize-1].ID)
	}

	return &storagev1.AdminListAuditLogsResponse{
		Logs:          entries,
		TotalCount:    int32(total),
		NextPageToken: nextPageToken,
	}, nil
}
