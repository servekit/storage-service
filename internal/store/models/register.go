package models

import (
	"database/sql"

	"gorm.io/cli/gorm/field"
	"gorm.io/cli/gorm/genconfig"
	"gorm.io/gorm"
)

// gormGenConfig configures `gorm gen` code generation. Discovered via AST
// scan (not runtime reflection), so the blank identifier is intentional —
// it signals "no runtime use, exists for the codegen tool only".
//
// See: gorm.io/cli/gorm/internal/gen/generator.go (tryParseConfig).
var _ = genconfig.Config{
	OutPath: "internal/store/generated",

	FieldTypeMap: map[any]any{
		sql.NullTime{}:   field.Time{},
		gorm.DeletedAt{}: field.Time{},
	},
}

// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&StorageObject{},
		&StorageFile{},
		&StorageQuota{},
		&StorageAuditLog{},
		&StorageUploadSession{},
	}
}
