// Package web holds the page, embedded in the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS is the page's files, rooted at static/.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
