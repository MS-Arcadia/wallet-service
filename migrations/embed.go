// Package migrations embeds the service's SQL schema migrations.
//
// They ship inside the binary rather than as a separate artefact, so the schema a
// container expects and the schema it applies can never drift apart. A pod that
// starts is a pod whose migrations matched its code.
//
// The package lives beside the .sql files because //go:embed can only reach files at
// or below the embedding package's own directory.
package migrations

import "embed"

// FS holds every migration file. Files must be named <version>_<description>.sql;
// pkg/migrate refuses anything else so that a mis-named file cannot be silently
// skipped.
//
//go:embed *.sql
var FS embed.FS

// Dir is the path within FS that pkg/migrate should read. The files sit at the root
// of the embedded filesystem.
const Dir = "."
