// Package option defines functional options for configuring the storage service.
package option

import (
	"github.com/servekit/storage-service/pkg/thirdcall"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Option configures a StorageService instance.
type Option func(*Options)

// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	Redis      *redis.Client
	GIDService thirdcall.GIDService
}

// WithDB provides an existing database connection.
func WithDB(db *gorm.DB) Option {
	return func(o *Options) { o.DB = db }
}

// WithRedis provides an existing Redis connection.
func WithRedis(client *redis.Client) Option {
	return func(o *Options) { o.Redis = client }
}

// WithGIDService provides a gid-service instance.
// If not set, the service creates one from config.ThirdParty.GID.
func WithGIDService(svc thirdcall.GIDService) Option {
	return func(o *Options) { o.GIDService = svc }
}

// Apply evaluates all options and returns the resolved Options.
func Apply(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
