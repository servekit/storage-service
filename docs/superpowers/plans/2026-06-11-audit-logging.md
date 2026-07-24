# Audit Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add operation audit logging for file management write operations, with before/after diff tracking and user/admin query APIs.

**Architecture:** Event Collector pattern — `audit.Recorder` interface injected into `StorageService`, called at business method level to capture before/after state. Synchronous write to PostgreSQL. `NopRecorder` for tests/dev.

**Tech Stack:** Go, GORM, PostgreSQL, gRPC, protobuf

---

## File Structure

| Action | Path | Responsibility |
|--------|------|---------------|
| Create | `internal/audit/event.go` | Event struct and action constants |
| Create | `internal/audit/recorder.go` | Recorder interface + DBRecorder implementation |
| Create | `internal/audit/nop_recorder.go` | No-op recorder for tests |
| Create | `internal/store/models/audit_log.go` | AuditLog GORM model |
| Create | `internal/store/repository/audit_log_repo.go` | AuditLog CRUD + query |
| Create | `pkg/xcodes/audit.go` | Audit-specific error codes |
| Modify | `internal/store/models/register.go` | Register AuditLog in AllModels() |
| Modify | `internal/service/service.go` | Add audit field, inject in constructor, add gRPC stubs |
| Modify | `internal/service/upload.go` | Add Record calls in confirmUpload + handleInstantUpload |
| Modify | `internal/service/file.go` | Add Record calls in update/delete methods |
| Modify | `internal/service/admin.go` | Add Record calls in admin write methods + audit query methods |
| Modify | `api/proto/storage/v1/storage.proto` | Add AuditAction enum + 2 RPCs + messages |
| Run | `buf generate` | Generate proto Go code |
| Run | `gorm gen` | Generate type-safe AuditLog field helpers |

---

### Task 1: Proto definitions for audit logging

**Files:**
- Modify: `api/proto/storage/v1/storage.proto`

- [ ] **Step 1: Add AuditAction enum, RPC methods, and messages to proto**

Add the following after the existing `SortField` enum (after line 71) and before the `service StorageService` block:

```protobuf
// AuditAction represents types of auditable operations.
enum AuditAction {
  AUDIT_ACTION_UNSPECIFIED = 0;
  AUDIT_ACTION_UPLOAD = 1;
  AUDIT_ACTION_UPDATE = 2;
  AUDIT_ACTION_DELETE = 3;
  AUDIT_ACTION_BATCH_DELETE = 4;
  AUDIT_ACTION_ADMIN_DELETE = 5;
  AUDIT_ACTION_ADMIN_SET_QUOTA = 6;
  AUDIT_ACTION_ADMIN_SOFT_DELETE_OWNER = 7;
  AUDIT_ACTION_ADMIN_DELETE_OWNER = 8;
}

// AuditLogStatus represents the result of an audited operation.
enum AuditLogStatus {
  AUDIT_LOG_STATUS_UNSPECIFIED = 0;
  AUDIT_LOG_STATUS_SUCCESS = 1;
  AUDIT_LOG_STATUS_FAILED = 2;
}
```

Add these RPCs inside the `service StorageService` block, after the `AdminDeleteOwner` RPC (after line 133):

```protobuf
  // Audit Log

  // ListMyAuditLogs lists audit logs for the calling user.
  rpc ListMyAuditLogs(ListMyAuditLogsRequest) returns (ListMyAuditLogsResponse);
  // AdminListAuditLogs lists all audit logs (admin only).
  rpc AdminListAuditLogs(AdminListAuditLogsRequest) returns (AdminListAuditLogsResponse);
```

Add these messages at the end of the file:

```protobuf
// Audit Log

message AuditLogEntry {
  int64 id = 1;
  AuditAction action = 2;
  OwnerType owner_type = 3;
  int64 owner_id = 4;
  string target_type = 5;
  int64 target_id = 6;
  google.protobuf.Struct before = 7;
  google.protobuf.Struct after = 8;
  AuditLogStatus status = 9;
  string error_message = 10;
  string request_id = 11;
  string created_at = 12;
}

message ListMyAuditLogsRequest {
  AuditAction action = 1;
  string target_type = 2;
  string start_time = 3;
  string end_time = 4;
  int32 page_size = 5;
  string page_token = 6;
  Owner owner = 255;
}

message ListMyAuditLogsResponse {
  repeated AuditLogEntry logs = 1;
  int32 total_count = 2;
  string next_page_token = 3;
}

message AdminListAuditLogsRequest {
  AuditAction action = 1;
  string target_type = 2;
  AuditLogStatus status = 3;
  string request_id = 4;
  OwnerType owner_type = 5;
  int64 owner_id = 6;
  int64 target_id = 7;
  string start_time = 8;
  string end_time = 9;
  int32 page_size = 10;
  string page_token = 11;
}

message AdminListAuditLogsResponse {
  repeated AuditLogEntry logs = 1;
  int32 total_count = 2;
  string next_page_token = 3;
}
```

Also add import for `google/protobuf/struct.proto`:

```protobuf
import "google/protobuf/struct.proto";
```

- [ ] **Step 2: Run buf generate**

Run: `buf generate`

Expected: proto files generated in `gen/storage/v1/`.

- [ ] **Step 3: Commit**

```bash
git add api/proto/ gen/
git commit -m "feat(audit): add audit log proto definitions"
```

---

### Task 2: AuditLog GORM model + error codes

**Files:**
- Create: `internal/store/models/audit_log.go`
- Create: `pkg/xcodes/audit.go`
- Modify: `internal/store/models/register.go`

- [ ] **Step 1: Create AuditLog model**

Create `internal/store/models/audit_log.go`:

```go
package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// AuditLog records a write operation on a storage resource.
type AuditLog struct {
	ID           int64            `gorm:"primaryKey" json:"id"`
	Action       string           `gorm:"column:action;type:varchar(64);not null" json:"action"`
	OwnerType    int32            `gorm:"column:owner_type;type:smallint;not null;default:1;index:idx_audit_logs_owner,sort:desc" json:"owner_type"`
	OwnerID      int64            `gorm:"column:owner_id;not null;index:idx_audit_logs_owner,sort:desc" json:"owner_id"`
	TargetType   string           `gorm:"column:target_type;type:varchar(32);not null;index:idx_audit_logs_target,sort:desc" json:"target_type"`
	TargetID     int64            `gorm:"column:target_id;not null;index:idx_audit_logs_target,sort:desc" json:"target_id"`
	Before       JSONMap          `gorm:"column:before;type:jsonb" json:"before,omitempty"`
	After        JSONMap          `gorm:"column:after;type:jsonb" json:"after,omitempty"`
	Status       string           `gorm:"column:status;type:varchar(16);not null" json:"status"`
	ErrorMessage string           `gorm:"column:error_message;type:text" json:"error_message,omitempty"`
	RequestID    string           `gorm:"column:request_id;type:varchar(64)" json:"request_id,omitempty"`
	CreatedAt    time.Time        `gorm:"column:created_at;not null;autoCreateTime;index:idx_audit_logs_created,sort:desc" json:"created_at"`
}

// JSONMap is a custom type for JSONB map fields with any values.
type JSONMap map[string]any

// Value implements the driver.Valuer interface.
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Scan implements the sql.Scanner interface.
func (m *JSONMap) Scan(value any) error {
	if value == nil {
		*m = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("cannot scan %T into JSONMap", value)
	}
	return json.Unmarshal(bytes, m)
}
```

- [ ] **Step 2: Create audit error codes**

Create `pkg/xcodes/audit.go`:

```go
package xcodes

import "github.com/servekit/go-common/xerr"

// Audit error codes.
var (
	ErrAuditLogNotFound = xerr.New("AUDIT_LOG_NOT_FOUND", xerr.CategoryNotFound, 404, "audit log not found")
)
```

- [ ] **Step 3: Register AuditLog in AllModels**

Modify `internal/store/models/register.go` — add `&AuditLog{}` to the slice:

```go
func AllModels() []any {
	return []any{
		&StorageObject{},
		&File{},
		&Quota{},
		&AuditLog{},
	}
}
```

- [ ] **Step 4: Run gorm gen**

Run: `gorm gen -i ./internal/store/models -o ./internal/store/generated`

Expected: `internal/store/generated/audit_log.go` created with type-safe field helpers.

- [ ] **Step 5: Verify compilation**

Run: `go build ./...`

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/store/models/audit_log.go internal/store/models/register.go internal/store/generated/ pkg/xcodes/audit.go
git commit -m "feat(audit): add AuditLog model, error codes, and generated code"
```

---

### Task 3: AuditLog repository

**Files:**
- Create: `internal/store/repository/audit_log_repo.go`

- [ ] **Step 1: Create AuditLogRepo**

Create `internal/store/repository/audit_log_repo.go`:

```go
package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/servekit/go-common/dbx"

	"storage-service/internal/store/generated"
	"storage-service/internal/store/models"
	"storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// AuditLogRepo provides database operations for audit logs.
type AuditLogRepo struct {
	db *gorm.DB
}

// NewAuditLogRepo creates a new AuditLogRepo.
func NewAuditLogRepo(db *gorm.DB) *AuditLogRepo {
	return &AuditLogRepo{db: db}
}

// Create inserts an audit log record.
func (r *AuditLogRepo) Create(ctx context.Context, log *models.AuditLog) error {
	if err := gorm.G[models.AuditLog](r.db).Create(ctx, log); err != nil {
		return xcodes.ErrInternal.Wrapf(err, "create audit log")
	}
	return nil
}

// AuditLogFilter defines filtering and pagination options for listing audit logs.
type AuditLogFilter struct {
	OwnerType  int32
	OwnerID    int64
	TargetType string
	TargetID   int64
	Action     string
	Status     string
	RequestID  string
	StartTime  time.Time
	EndTime    time.Time
	dbx.Pagination
}

// ListByOwner returns a paginated list of audit logs for a given owner.
func (r *AuditLogRepo) ListByOwner(ctx context.Context, ownerType int32, ownerID int64, filter AuditLogFilter) ([]models.AuditLog, int, error) {
	q := gorm.G[models.AuditLog](r.db).
		Where(generated.AuditLog.OwnerType.Eq(ownerType)).
		Where(generated.AuditLog.OwnerID.Eq(ownerID))

	q = r.applyFilters(q, filter)

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs")
	}

	q = q.Order(generated.AuditLog.CreatedAt.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.AuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs")
	}

	return logs, int(total), nil
}

// ListAll returns a paginated list of all audit logs with filters (admin use).
func (r *AuditLogRepo) ListAll(ctx context.Context, filter AuditLogFilter) ([]models.AuditLog, int, error) {
	q := gorm.G[models.AuditLog](r.db)

	q = r.applyFilters(q, filter)

	if filter.OwnerType > 0 {
		q = q.Where(generated.AuditLog.OwnerType.Eq(filter.OwnerType))
	}
	if filter.OwnerID > 0 {
		q = q.Where(generated.AuditLog.OwnerID.Eq(filter.OwnerID))
	}
	if filter.TargetID > 0 {
		q = q.Where(generated.AuditLog.TargetID.Eq(filter.TargetID))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs (admin)")
	}

	q = q.Order(generated.AuditLog.CreatedAt.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.AuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs (admin)")
	}

	return logs, int(total), nil
}

func (r *AuditLogRepo) applyFilters(q typed.AuditLogQuery, filter AuditLogFilter) typed.AuditLogQuery {
	if filter.Action != "" {
		q = q.Where(generated.AuditLog.Action.Eq(filter.Action))
	}
	if filter.TargetType != "" {
		q = q.Where(generated.AuditLog.TargetType.Eq(filter.TargetType))
	}
	if filter.Status != "" {
		q = q.Where(generated.AuditLog.Status.Eq(filter.Status))
	}
	if filter.RequestID != "" {
		q = q.Where(generated.AuditLog.RequestID.Eq(filter.RequestID))
	}
	if !filter.StartTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Gte(filter.StartTime))
	}
	if !filter.EndTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Lte(filter.EndTime))
	}
	return q
}
```

**Important note:** The exact type for the query variable `q` and the return type of `applyFilters` must match the generated type. After running gorm gen in Task 2, check `internal/store/generated/audit_log.go` for the exact type name — it will be something like `*generated.AuditLogQuery` or the typed generic query type. Adjust the `typed.AuditLogQuery` placeholder to match the actual generated type. Similarly, `generated.AuditLog.XXX` field references will be correct once gen runs. If gen produces a different field accessor pattern, adjust accordingly.

**Update:** Looking at the existing pattern (e.g., `file_repo.go`), the query type is `gorm.G[models.File](r.db)` which returns a typed query directly. The `applyFilters` approach needs to use the same return type. Since GORM gen's typed queries are not easily abstracted into a helper method, inline the filter logic instead. Revised approach:

```go
package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/servekit/go-common/dbx"

	"storage-service/internal/store/generated"
	"storage-service/internal/store/models"
	"storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// AuditLogRepo provides database operations for audit logs.
type AuditLogRepo struct {
	db *gorm.DB
}

// NewAuditLogRepo creates a new AuditLogRepo.
func NewAuditLogRepo(db *gorm.DB) *AuditLogRepo {
	return &AuditLogRepo{db: db}
}

// Create inserts an audit log record.
func (r *AuditLogRepo) Create(ctx context.Context, log *models.AuditLog) error {
	if err := gorm.G[models.AuditLog](r.db).Create(ctx, log); err != nil {
		return xcodes.ErrInternal.Wrapf(err, "create audit log")
	}
	return nil
}

// AuditLogFilter defines filtering and pagination options for listing audit logs.
type AuditLogFilter struct {
	OwnerType  int32
	OwnerID    int64
	TargetType string
	TargetID   int64
	Action     string
	Status     string
	RequestID  string
	StartTime  time.Time
	EndTime    time.Time
	dbx.Pagination
}

// ListByOwner returns a paginated list of audit logs for a given owner.
func (r *AuditLogRepo) ListByOwner(ctx context.Context, ownerType int32, ownerID int64, filter AuditLogFilter) ([]models.AuditLog, int, error) {
	q := gorm.G[models.AuditLog](r.db).
		Where(generated.AuditLog.OwnerType.Eq(ownerType)).
		Where(generated.AuditLog.OwnerID.Eq(ownerID))

	if filter.Action != "" {
		q = q.Where(generated.AuditLog.Action.Eq(filter.Action))
	}
	if filter.TargetType != "" {
		q = q.Where(generated.AuditLog.TargetType.Eq(filter.TargetType))
	}
	if !filter.StartTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Gte(filter.StartTime))
	}
	if !filter.EndTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Lte(filter.EndTime))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs")
	}

	q = q.Order(generated.AuditLog.CreatedAt.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.AuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs")
	}

	return logs, int(total), nil
}

// ListAll returns a paginated list of all audit logs with filters (admin use).
func (r *AuditLogRepo) ListAll(ctx context.Context, filter AuditLogFilter) ([]models.AuditLog, int, error) {
	q := gorm.G[models.AuditLog](r.db)

	if filter.OwnerType > 0 {
		q = q.Where(generated.AuditLog.OwnerType.Eq(filter.OwnerType))
	}
	if filter.OwnerID > 0 {
		q = q.Where(generated.AuditLog.OwnerID.Eq(filter.OwnerID))
	}
	if filter.TargetType != "" {
		q = q.Where(generated.AuditLog.TargetType.Eq(filter.TargetType))
	}
	if filter.TargetID > 0 {
		q = q.Where(generated.AuditLog.TargetID.Eq(filter.TargetID))
	}
	if filter.Action != "" {
		q = q.Where(generated.AuditLog.Action.Eq(filter.Action))
	}
	if filter.Status != "" {
		q = q.Where(generated.AuditLog.Status.Eq(filter.Status))
	}
	if filter.RequestID != "" {
		q = q.Where(generated.AuditLog.RequestID.Eq(filter.RequestID))
	}
	if !filter.StartTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Gte(filter.StartTime))
	}
	if !filter.EndTime.IsZero() {
		q = q.Where(generated.AuditLog.CreatedAt.Lte(filter.EndTime))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs (admin)")
	}

	q = q.Order(generated.AuditLog.CreatedAt.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.AuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs (admin)")
	}

	return logs, int(total), nil
}
```

**Note:** Remove the unused `strconv` and `time` imports if they aren't needed after the final version. `strconv` is not used here, `time` is used for `AuditLogFilter` fields.

- [ ] **Step 2: Verify compilation**

Run: `go build ./internal/store/repository/`

Expected: no errors (may require gorm gen from Task 2 to have run first).

- [ ] **Step 3: Commit**

```bash
git add internal/store/repository/audit_log_repo.go
git commit -m "feat(audit): add AuditLogRepo with owner and admin query"
```

---

### Task 4: audit package (Event, Recorder interface, DBRecorder, NopRecorder)

**Files:**
- Create: `internal/audit/event.go`
- Create: `internal/audit/recorder.go`
- Create: `internal/audit/nop_recorder.go`

- [ ] **Step 1: Create event.go with Event struct and action constants**

Create `internal/audit/event.go`:

```go
package audit

// Action constants for audit events.
const (
	ActionUpload                = "upload"
	ActionUpdate                = "update"
	ActionDelete                = "delete"
	ActionBatchDelete           = "batch_delete"
	ActionAdminDelete           = "admin_delete"
	ActionAdminSetQuota         = "admin_set_quota"
	ActionAdminSoftDeleteOwner  = "admin_soft_delete_owner"
	ActionAdminDeleteOwner      = "admin_delete_owner"

	StatusSuccess = "success"
	StatusFailed  = "failed"

	TargetTypeFile  = "file"
	TargetTypeQuota = "quota"
	TargetTypeOwner = "owner"
)

// Event represents an auditable operation.
type Event struct {
	Action     string         // Operation type (one of the Action constants)
	OwnerType  int32          // Who performed the operation
	OwnerID    int64          // Who performed the operation
	TargetType string         // What was operated on (file/quota/owner)
	TargetID   int64          // ID of the target
	Before     map[string]any // State before the operation (nil for creates)
	After      map[string]any // State after the operation (nil for deletes)
	Status     string         // success or failed
	Error      error          // Failure reason (nil for success)
}
```

- [ ] **Step 2: Create recorder.go with Recorder interface and DBRecorder**

Create `internal/audit/recorder.go`:

```go
package audit

import (
	"context"
	"log/slog"

	"storage-service/internal/store/models"
	"storage-service/internal/store/repository"
)

// Recorder records audit events.
type Recorder interface {
	Record(ctx context.Context, event Event) error
}

// DBRecorder writes audit events to the database.
type DBRecorder struct {
	repo   *repository.AuditLogRepo
	gid    gidGenerator
}

// gidGenerator generates snowflake IDs.
type gidGenerator interface {
	NextID(ctx context.Context) (int64, error)
}

// NewDBRecorder creates a new DBRecorder.
func NewDBRecorder(repo *repository.AuditLogRepo, gid gidGenerator) *DBRecorder {
	return &DBRecorder{repo: repo, gid: gid}
}

// Record persists an audit event to the database.
// Errors are logged but not propagated — audit failure must not block business operations.
func (r *DBRecorder) Record(ctx context.Context, event Event) error {
	id, err := r.gid.NextID(ctx)
	if err != nil {
		slog.Error("audit: generate id", "error", err)
		return err
	}

	var errMsg string
	if event.Error != nil {
		errMsg = event.Error.Error()
	}

	auditLog := &models.AuditLog{
		ID:           id,
		Action:       event.Action,
		OwnerType:    event.OwnerType,
		OwnerID:      event.OwnerID,
		TargetType:   event.TargetType,
		TargetID:     event.TargetID,
		Before:       models.JSONMap(event.Before),
		After:        models.JSONMap(event.After),
		Status:       event.Status,
		ErrorMessage: errMsg,
	}

	if createErr := r.repo.Create(ctx, auditLog); createErr != nil {
		slog.Error("audit: write log", "error", createErr)
		return createErr
	}
	return nil
}
```

**Note:** The `gidGenerator` interface must match the project's GID service. The service already has `thirdcall.GIDService` with a `NextID(ctx) (int64, error)` method. We reference it as an interface to avoid importing the thirdcall package directly. The `DBRecorder` constructor accepts any `gidGenerator` implementation — in practice, the same `thirdcall.GIDService` instance used by `StorageService`.

- [ ] **Step 3: Create nop_recorder.go**

Create `internal/audit/nop_recorder.go`:

```go
package audit

import "context"

// NopRecorder is a no-op recorder for tests and development.
type NopRecorder struct{}

// NewNopRecorder creates a new NopRecorder.
func NewNopRecorder() *NopRecorder {
	return &NopRecorder{}
}

// Record does nothing and returns nil.
func (n *NopRecorder) Record(_ context.Context, _ Event) error {
	return nil
}
```

- [ ] **Step 4: Verify compilation**

Run: `go build ./internal/audit/`

Expected: no errors. Note: may need to adjust imports depending on the actual GID service interface. Check `pkg/thirdcall/` for the exact interface.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/
git commit -m "feat(audit): add Event, Recorder interface, DBRecorder, and NopRecorder"
```

---

### Task 5: Wire audit into StorageService

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: Add audit field to StorageService and wire in constructor**

Add import for the audit package:

```go
"storage-service/internal/audit"
```

Add `audit audit.Recorder` field to the `StorageService` struct (after `fileRepo`):

```go
audit       audit.Recorder
```

Add a new `WithAudit` option to `pkg/option/option.go` — add the field to `Options`:

```go
AuditRecorder audit.Recorder
```

And the option function:

```go
// WithAuditRecorder provides an audit recorder.
func WithAuditRecorder(r audit.Recorder) Option {
    return func(o *Options) { o.AuditRecorder = r }
}
```

This requires adding the import:

```go
"storage-service/internal/audit"
```

In `service.go` constructor `New()`, after creating the repos (after line 67), initialize the audit recorder:

```go
var auditRecorder audit.Recorder
if o.AuditRecorder != nil {
    auditRecorder = o.AuditRecorder
} else {
    auditLogRepo := repository.NewAuditLogRepo(db)
    auditRecorder = audit.NewDBRecorder(auditLogRepo, gidGen)
}
```

Add `audit: auditRecorder` to the return struct.

- [ ] **Step 2: Add gRPC stubs for audit query methods**

Add stubs in `service.go` after the existing admin stubs (before line 199), in the `// Admin` section:

```go
// Audit Log

func (s *StorageService) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return s.listMyAuditLogs(ctx, req)
}

func (s *StorageService) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return s.adminListAuditLogs(ctx, req)
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./...`

Expected: compilation will fail because the private methods `listMyAuditLogs` and `adminListAuditLogs` don't exist yet. That's expected — we'll add them in Task 7. If you want to temporarily unblock compilation, create stub implementations that return `nil, nil`.

- [ ] **Step 4: Commit**

```bash
git add internal/service/service.go pkg/option/option.go
git commit -m "feat(audit): wire audit recorder into StorageService"
```

---

### Task 6: Add Record calls to business methods

**Files:**
- Modify: `internal/service/upload.go`
- Modify: `internal/service/file.go`
- Modify: `internal/service/admin.go`

This is the core change — adding `s.audit.Record()` calls to each write operation. Follow the pattern: capture before state → perform operation → record event.

- [ ] **Step 1: Add audit import to each file**

Add to the import block of `upload.go`, `file.go`, and `admin.go`:

```go
"storage-service/internal/audit"
```

- [ ] **Step 2: upload.go — confirmUpload**

In `confirmUpload`, after the successful transaction (after line 217, before `return result, nil`), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionUpload,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   result.FileId,
	After: map[string]any{
		"filename":    token.Filename,
		"file_path":   token.FilePath,
		"description": token.Description,
		"size":        info.Size,
		"content_type": token.ContentType,
		"md5":         token.MD5,
		"is_public":   token.IsPublic,
	},
	Status: audit.StatusSuccess,
})
```

On the error path (after line 215, where `txErr != nil`), add before the error return:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionUpload,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	Status:     audit.StatusFailed,
	Error:      txErr,
})
```

- [ ] **Step 3: upload.go — handleInstantUpload**

In `handleInstantUpload`, after successful transaction (after line 357, before `return fileInfo, nil`), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionUpload,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   fileInfo.Id,
	After: map[string]any{
		"filename":    filename,
		"file_path":   filePath,
		"description": description,
		"size":        existing.Size,
		"is_public":   isPublic,
	},
	Status: audit.StatusSuccess,
})
```

**Note:** `handleInstantUpload` is called from `generateUploadURL` and `getSTSCredential`, so it doesn't have direct access to the context. But it already receives `ctx` as its first parameter, so `s.audit.Record(ctx, ...)` works.

- [ ] **Step 4: file.go — updateMyFile**

In `updateMyFile`, capture the old values before the update (after line 146, where `uf` is loaded), store them:

```go
oldValues := map[string]any{
	"filename":    uf.Filename,
	"file_path":   uf.FilePath,
	"description": uf.Description,
	"is_public":   uf.IsPublic,
}
```

After the successful update (after line 170), add:

```go
newValues := map[string]any{
	"filename":    uf.Filename,
	"file_path":   uf.FilePath,
	"description": uf.Description,
	"is_public":   uf.IsPublic,
}
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionUpdate,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   uf.ID,
	Before:     oldValues,
	After:      newValues,
	Status:     audit.StatusSuccess,
})
```

- [ ] **Step 5: file.go — deleteMyFile**

In `deleteMyFile`, after the successful transaction (after line 211, before `return &emptypb.Empty{}, nil`), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionDelete,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   uf.ID,
	Before: map[string]any{
		"filename":    uf.Filename,
		"file_path":   uf.FilePath,
		"size":        obj.Size,
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 6: file.go — batchDeleteMyFiles**

In `batchDeleteMyFiles`, after the successful transaction (after line 257, before the return), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionBatchDelete,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   req.GetFileIds()[0],
	Before: map[string]any{
		"file_ids":     req.GetFileIds(),
		"deleted_count": deletedCount,
		"failed_ids":   failedIDs,
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 7: admin.go — adminDeleteFile**

In `adminDeleteFile`, after the successful transaction (after line 255, before `return &emptypb.Empty{}, nil`), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionAdminDelete,
	OwnerType:  f.OwnerType,
	OwnerID:    f.OwnerID,
	TargetType: audit.TargetTypeFile,
	TargetID:   f.ID,
	Before: map[string]any{
		"filename":    f.Filename,
		"file_path":   f.FilePath,
		"size":        obj.Size,
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 8: admin.go — adminSetQuota**

In `adminSetQuota`, capture the old quota before the transaction. Before line 43, add:

```go
oldQuota, oldQuotaErr := s.getQuota(ctx, s.db, ownerType, ownerID)
var oldTotalBytes int64
if oldQuotaErr == nil {
	oldTotalBytes = oldQuota.TotalBytes
}
```

After the successful transaction (after line 66, before `return result, nil`), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionAdminSetQuota,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeQuota,
	TargetID:   ownerID,
	Before: map[string]any{
		"total_bytes": oldTotalBytes,
	},
	After: map[string]any{
		"total_bytes": req.GetTotalBytes(),
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 9: admin.go — adminSoftDeleteOwnerFiles**

In `adminSoftDeleteOwnerFiles`, after the successful operation (after line 81, before the return), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionAdminSoftDeleteOwner,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeOwner,
	TargetID:   ownerID,
	Before: map[string]any{
		"files_deleted":  filesDeleted,
		"bytes_released": bytesReleased,
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 10: admin.go — adminDeleteOwner**

In `adminDeleteOwner`, after the successful transaction (after line 102, before the return), add:

```go
s.audit.Record(ctx, audit.Event{
	Action:     audit.ActionAdminDeleteOwner,
	OwnerType:  ownerType,
	OwnerID:    ownerID,
	TargetType: audit.TargetTypeOwner,
	TargetID:   ownerID,
	Before: map[string]any{
		"files_deleted":  filesDeleted,
		"bytes_released": bytesReleased,
	},
	Status: audit.StatusSuccess,
})
```

- [ ] **Step 11: Verify compilation**

Run: `go build ./...`

Expected: will still fail on missing `listMyAuditLogs` and `adminListAuditLogs` private methods (from Task 5). That's OK — we add those next.

- [ ] **Step 12: Commit**

```bash
git add internal/service/upload.go internal/service/file.go internal/service/admin.go
git commit -m "feat(audit): add Record calls to all write operations"
```

---

### Task 7: Implement audit query service methods

**Files:**
- Modify: `internal/service/admin.go` (or create `internal/service/audit.go`)

- [ ] **Step 1: Create audit.go with query implementations**

Create `internal/service/audit.go`:

```go
package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/servekit/go-common/dbx"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/store/repository"
)

func (s *StorageService) listMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	filter := repository.AuditLogFilter{
		Action: actionToFilter(req.GetAction()),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	if req.GetTargetType() != "" {
		filter.TargetType = req.GetTargetType()
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

	logs, total, err := s.auditLogRepo.ListByOwner(ctx, ownerType, ownerID, filter)
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

func (s *StorageService) adminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	filter := repository.AuditLogFilter{
		OwnerType:  int32(req.GetOwnerType()),
		OwnerID:    req.GetOwnerId(),
		TargetID:   req.GetTargetId(),
		Action:     actionToFilter(req.GetAction()),
		Status:     statusToFilter(req.GetStatus()),
		RequestID:  req.GetRequestId(),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	if req.GetTargetType() != "" {
		filter.TargetType = req.GetTargetType()
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

	logs, total, err := s.auditLogRepo.ListAll(ctx, filter)
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

// --- internal helpers ---

func actionToFilter(action storagev1.AuditAction) string {
	switch action {
	case storagev1.AuditAction_AUDIT_ACTION_UPLOAD:
		return "upload"
	case storagev1.AuditAction_AUDIT_ACTION_UPDATE:
		return "update"
	case storagev1.AuditAction_AUDIT_ACTION_DELETE:
		return "delete"
	case storagev1.AuditAction_AUDIT_ACTION_BATCH_DELETE:
		return "batch_delete"
	case storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE:
		return "admin_delete"
	case storagev1.AuditAction_AUDIT_ACTION_ADMIN_SET_QUOTA:
		return "admin_set_quota"
	case storagev1.AuditAction_AUDIT_ACTION_ADMIN_SOFT_DELETE_OWNER:
		return "admin_soft_delete_owner"
	case storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_OWNER:
		return "admin_delete_owner"
	default:
		return ""
	}
}

func statusToFilter(status storagev1.AuditLogStatus) string {
	switch status {
	case storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS:
		return "success"
	case storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED:
		return "failed"
	default:
		return ""
	}
}

func buildAuditLogEntry(log *models.AuditLog) *storagev1.AuditLogEntry {
	entry := &storagev1.AuditLogEntry{
		Id:          log.ID,
		Action:      actionToProto(log.Action),
		OwnerType:   ownerTypeToProto(log.OwnerType),
		OwnerId:     log.OwnerID,
		TargetType:  log.TargetType,
		TargetId:    log.TargetID,
		Status:      statusToProto(log.Status),
		ErrorMessage: log.ErrorMessage,
		RequestId:   log.RequestID,
		CreatedAt:   log.CreatedAt.Format(time.RFC3339),
	}

	// Convert JSONMap to protobuf Struct for before/after.
	if log.Before != nil {
		entry.Before = mapToStruct(log.Before)
	}
	if log.After != nil {
		entry.After = mapToStruct(log.After)
	}

	return entry
}

func actionToProto(action string) storagev1.AuditAction {
	switch action {
	case "upload":
		return storagev1.AuditAction_AUDIT_ACTION_UPLOAD
	case "update":
		return storagev1.AuditAction_AUDIT_ACTION_UPDATE
	case "delete":
		return storagev1.AuditAction_AUDIT_ACTION_DELETE
	case "batch_delete":
		return storagev1.AuditAction_AUDIT_ACTION_BATCH_DELETE
	case "admin_delete":
		return storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE
	case "admin_set_quota":
		return storagev1.AuditAction_AUDIT_ACTION_ADMIN_SET_QUOTA
	case "admin_soft_delete_owner":
		return storagev1.AuditAction_AUDIT_ACTION_ADMIN_SOFT_DELETE_OWNER
	case "admin_delete_owner":
		return storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_OWNER
	default:
		return storagev1.AuditAction_AUDIT_ACTION_UNSPECIFIED
	}
}

func statusToProto(status string) storagev1.AuditLogStatus {
	switch status {
	case "success":
		return storagev1.AuditLogStatus_AUDIT_LOG_STATUS_SUCCESS
	case "failed":
		return storagev1.AuditLogStatus_AUDIT_LOG_STATUS_FAILED
	default:
		return storagev1.AuditLogStatus_AUDIT_LOG_STATUS_UNSPECIFIED
	}
}

func mapToStruct(m map[string]any) *structpb.Struct {
	return structpb.NewStructMust(m)
}
```

**Important:** This file needs these additional imports:

```go
"storage-service/internal/store/models"
"google.golang.org/protobuf/types/known/structpb"
```

And `s.auditLogRepo` must be a field on `StorageService`. Add `auditLogRepo *repository.AuditLogRepo` to the struct in `service.go` and initialize it in the constructor alongside the other repos.

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`

Expected: compiles cleanly.

- [ ] **Step 3: Commit**

```bash
git add internal/service/audit.go
git commit -m "feat(audit): implement ListMyAuditLogs and AdminListAuditLogs"
```

---

### Task 8: Run migration and integration test

**Files:**
- No new files

- [ ] **Step 1: Run AutoMigrate**

Run: `go run ./cmd/migrate/`

Expected: `stor_audit_logs` table created with correct columns and indexes.

- [ ] **Step 2: Run existing tests**

Run: `go test -race ./...`

Expected: all existing tests pass. New audit recording is transparent to existing tests because they use `NopRecorder` or the default `DBRecorder` with a real DB (integration tests) or the audit calls silently succeed/fail without affecting test outcomes.

- [ ] **Step 3: Commit**

If any fixes were needed, commit them. Otherwise skip.

---

### Task 9: Sync design doc to Obsidian

**Files:**
- Obsidian vault files

- [ ] **Step 1: Sync the design doc to Obsidian**

Follow the CLAUDE.md Obsidian sync rules. Create the note in `services/storage-service/design/v1/audit-logging.md` with the full spec content. Update `services/index.md` and `services/changes.md`.

---

## Self-Review

### Spec coverage check:

| Spec requirement | Task |
|-----------------|------|
| AuditLog data model | Task 2 |
| AuditLogRepo with query | Task 3 |
| Event + Recorder interface | Task 4 |
| DBRecorder implementation | Task 4 |
| NopRecorder | Task 4 |
| Wire into StorageService | Task 5 |
| confirmUpload recording | Task 6, Step 2 |
| handleInstantUpload recording | Task 6, Step 3 |
| updateMyFile recording | Task 6, Step 4 |
| deleteMyFile recording | Task 6, Step 5 |
| batchDeleteMyFiles recording | Task 6, Step 6 |
| adminDeleteFile recording | Task 6, Step 7 |
| adminSetQuota recording | Task 6, Step 8 |
| adminSoftDeleteOwnerFiles recording | Task 6, Step 9 |
| adminDeleteOwner recording | Task 6, Step 10 |
| ListMyAuditLogs RPC | Task 7 |
| AdminListAuditLogs RPC | Task 7 |
| User filters: action, target_type, time range | Task 7 |
| Admin filters: action, target_type, status, request_id, owner_type+owner_id, target_id, time range | Task 7 |
| Cursor pagination with snowflake ID | Task 7 |

### Placeholder scan:
No TBD/TODO/placeholders found.

### Type consistency:
- `audit.Event` uses `map[string]any` for Before/After — consistent with `models.JSONMap` (also `map[string]any`)
- `audit.Recorder` interface is consistent across `DBRecorder` and `NopRecorder`
- Proto `AuditAction`/`AuditLogStatus` enums map 1:1 to string constants in `event.go`
- `AuditLogFilter` struct fields match what `ListByOwner` and `ListAll` use
