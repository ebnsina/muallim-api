// Package migrations embeds the SQL migration files so the binary carries its
// own schema history and production needs no separate migration tool.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
