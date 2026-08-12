// Package migrations embeds the SQL migrations of the API-owned schema so the
// binary can apply them at boot without shipping files alongside it.
package migrations

import "embed"

// FS holds every migration file, consumed through golang-migrate's iofs source.
//
//go:embed *.sql
var FS embed.FS
