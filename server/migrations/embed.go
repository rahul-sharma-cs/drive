// Package migrations carries the goose SQL migrations into the binary.
//
// The embed directive has to sit next to the files it embeds -- Go rejects
// ".." in embed patterns -- so this file exists purely to hand the .sql files
// to internal/db.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
