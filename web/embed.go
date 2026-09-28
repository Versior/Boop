// Package web embeds the templates and static assets served by the Boop binary.
package web

import "embed"

// FS holds the server-rendered templates and the static assets. Everything the
// binary serves ships inside the binary; there is no external asset directory.
//
//go:embed templates static
var FS embed.FS
