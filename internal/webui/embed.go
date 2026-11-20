// Package webui — собранный фронтенд (web/ → internal/webui/dist, make web),
// встроенный в ramplus-web через go:embed.
package webui

import (
	"embed"
	"io/fs"
)

// dist содержит .keep, чтобы пакет собирался и без сборки фронтенда.
//
//go:embed all:dist
var dist embed.FS

// Assets — файлы фронтенда (index.html, assets/...).
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
