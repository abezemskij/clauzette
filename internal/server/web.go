package server

import (
	"embed"
	"net/http"
)

// The web page: the same client as clauzette attach, in a browser. It
// holds no secrets, so it loads without the token; the page asks for the
// token and sends it with every API request.
//
//go:embed web/index.html web/app.css web/app.js
var webFiles embed.FS

// pageCSP allows only this server's own script, style and API: no inline
// script, nothing from elsewhere, no framing.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: blob:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func webFile(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFiles.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}
