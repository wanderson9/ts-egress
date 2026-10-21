// Package ui serves the prebuilt React application. Node is build-time only.
package ui

import (
	"bytes"
	"embed"
	"html"
	"net/http"
	"strings"
)

//go:embed dist/*
var assets embed.FS

func Serve(w http.ResponseWriter, r *http.Request, page string) {
	name := r.URL.Path
	switch name {
	case "/", "/index.html", "/dashboard", "/setup", "/settings", "/password":
		name = "/index.html"
	case "/app.js", "/app.css":
	default:
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("dist" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch name {
	case "/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case "/app.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		policy := w.Header().Get("Content-Security-Policy")
		if _, rest, ok := strings.Cut(policy, "'nonce-"); ok {
			nonce, _, _ := strings.Cut(rest, "'")
			meta := `<meta name="csp-nonce" content="` + html.EscapeString(nonce) + `">`
			b = bytes.Replace(b, []byte("<head>"), []byte("<head>"+meta), 1)
		}
		b = bytes.ReplaceAll(b, []byte("__PAGE__"), []byte(page))
		title := map[string]string{"dashboard": "Services", "settings": "Settings", "setup": "Gateway setup"}[page]
		if r.URL.Path == "/password" {
			title = "Change password"
		}
		if title == "" {
			title = "Gateway"
		}
		start, end := bytes.Index(b, []byte("<title>")), bytes.Index(b, []byte("</title>"))
		if start >= 0 && end > start {
			b = append(append(append([]byte{}, b[:start+7]...), []byte(html.EscapeString(title+" · ts-egress"))...), b[end:]...)
		}
	}
	w.Write(b)
}
