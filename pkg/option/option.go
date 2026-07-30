// Package option defines functional options for configuring the storage service.
package option

import (
	gidservice "github.com/servekit/gid-service/pkg"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Option configures a StorageService instance.
type Option func(*Options)

// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	Redis      *redis.Client
	GIDHandler *gidservice.Handler
}

// WithDB provides an existing database connection.
func WithDB(db *gorm.DB) Option {
	return func(o *Options) { o.DB = db }
}

// WithRedis provides an existing Redis connection.
func WithRedis(client *redis.Client) Option {
	return func(o *Options) { o.Redis = client }
}

// WithGIDHandler injects a raw gid-service Handler. StorageService wraps it
// internally into its GIDService; callers do not need to know that interface.
// Required when third_party.gid.mode=module (a parent process embeds this
// service and owns the Handler). In grpc mode the service dials gid-service
// itself and this option is ignored.
func WithGIDHandler(h *gidservice.Handler) Option {
	return func(o *Options) { o.GIDHandler = h }
}

// Apply evaluates all options and returns the resolved Options.
func Apply(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
