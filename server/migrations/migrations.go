package migrations

import "embed"

// Files contains versioned SQL migrations. Applied files must not be edited.
//
//go:embed *.sql
var Files embed.FS

// Latest is the schema version the application expects. The readiness probe compares against it,
// so a new migration must bump it in the same change that adds the file.
const Latest uint = 4
