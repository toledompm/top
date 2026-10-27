package sqlite

import (
	"embed"
	"io/fs"
)

// migrationsFS embeds the forward-only goose migration files (single flat
// directory; files are named NNNN_<something>.sql). goose reads the
// provided fs at its root, so the embedded `migrations/` subtree is
// unwrapped via fs.Sub before handoff.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

func init() {
	if sub, err := fs.Sub(migrationsFS, "migrations"); err == nil {
		migrations = sub
	}
}

// migrations is the root fsys passed to goose.NewProvider.
var migrations fs.FS
