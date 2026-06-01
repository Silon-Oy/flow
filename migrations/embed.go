// Package migrations embeds the SQL migration files so flowd can run them on
// boot without shipping a separate migrate CLI or the .sql files alongside the
// binary.
package migrations

import "embed"

// FS holds all .sql migration files (golang-migrate naming convention:
// NNNNNN_name.up.sql / .down.sql).
//
//go:embed *.sql
var FS embed.FS
