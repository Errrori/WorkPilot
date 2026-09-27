// Package webui serves the optional demo page used for manual verification.
package webui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static/*
var assets embed.FS

// Mount serves the demo UI under /ui and redirects / to /ui/.
func Mount(r *gin.Engine) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/ui/")
	})
	r.StaticFS("/ui", http.FS(sub))
}
