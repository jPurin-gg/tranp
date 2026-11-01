// Package web embeds the frontend so the server is a single binary.
package web

import "embed"

//go:embed index.html room.html manifest.webmanifest static
var Files embed.FS
