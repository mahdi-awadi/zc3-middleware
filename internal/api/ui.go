package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed webui
var webuiFS embed.FS

// uiHandler serves the embedded admin web UI (files under webui/).
func uiHandler() http.Handler {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		panic(err) // embed guarantees the dir exists at build time
	}
	return http.FileServer(http.FS(sub))
}
