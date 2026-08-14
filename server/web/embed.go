// Package web carries the built SPA into the binary.
//
// The embed directive has to live next to the directory it embeds — Go rejects
// ".." in embed patterns — so this file exists purely to hand `dist` to
// cmd/drive. The committed placeholder index.html keeps it compiling before the
// first `make build`.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
