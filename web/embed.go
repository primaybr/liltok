package web

import "embed"

// DistFS embeds the web dashboard SPA assets into the compiled Go binary.
//
//go:embed index.html
var DistFS embed.FS
