// Package webui carries the browser assets, compiled into the binary.
//
// Embedding is what makes the tool a single file with nothing to install at
// the far end, and it also guarantees the page can never pull anything off the
// network: every byte the browser sees ships inside the executable.
package webui

import "embed"

//go:embed assets
var FS embed.FS
