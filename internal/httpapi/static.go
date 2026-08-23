package httpapi

import (
	"embed"
	"net/http"
)

// The vendored chart library is uPlot v1.6.32 (https://github.com/leeoniya/uPlot),
// MIT license; the license text is kept beside it in static/uplot.LICENSE.
//
//go:embed static/*.js static/*.css
var staticFiles embed.FS

func serveStatic(name, contentType string) http.HandlerFunc {
	content, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		panic("missing embedded static file " + name)
	}
	return func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", contentType)
		response.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = response.Write(content)
	}
}
