package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var uiFS embed.FS

// uiHandler serves the embedded web monitor (the ui/ subtree) at /.
func uiHandler() (http.Handler, error) {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
