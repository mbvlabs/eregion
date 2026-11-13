// Package dbschema owns the embedded, fresh-install platform schema.
package dbschema

import "embed"

// GooseFS contains the migrations in Goose's expected migrations directory.
//
//go:embed migrations/*.sql
var GooseFS embed.FS
