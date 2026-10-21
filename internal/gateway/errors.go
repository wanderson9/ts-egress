package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strconv"
)

const errorCSS = `:root{color-scheme:light dark;font:15px/1.6 system-ui,-apple-system,sans-serif;background:#f6f7f8;color:#252b33}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px}main{width:100%;max-width:480px;border-top:2px solid #667482;padding:24px 0}small{font:13px ui-monospace,monospace;color:#65717e}h1{font-size:28px;line-height:1.3;letter-spacing:-.5px;margin:12px 0}p{color:#596572;margin:12px 0}a{display:inline-block;color:inherit;border:1px solid #aeb7c1;border-radius:4px;padding:7px 14px;text-decoration:none;margin-top:12px}a:hover{background:#e6e9ed}a:focus-visible{outline:2px solid #477aa7;outline-offset:4px}@media(prefers-color-scheme:dark){:root{background:#111418;color:#dce1e6}small,p{color:#9da7b2}a{border-color:#485360}a:hover{background:#21262d}}`

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Code}} · {{.Title}}</title><style>` + errorCSS + `</style></head><body><main><small>HTTP {{.Code}}</small><h1>{{.Title}}</h1><p>{{.Message}}</p>{{if .Retry}}<a href="">Try again</a>{{end}}</main></body></html>`))

type errorContent struct {
	Code           int
	Title, Message string
	Retry          bool
}

var errorPages = func() map[int][]byte {
	pages := []errorContent{
		{404, "Site not found", "No public service is available at this address.", false},
		{403, "Access denied", "This request is not allowed.", false},
		{421, "Connection rejected", "The connection does not match the requested site. Open the site using its public domain.", false},
		{502, "Service unavailable", "The service could not be reached. Try again later.", true},
		{503, "Service unavailable", "This service is not ready to accept requests. Try again later.", true},
		{504, "Service timed out", "The service did not respond in time. Try again later.", true},
	}
	out := map[int][]byte{}
	for _, p := range pages {
		var b bytes.Buffer
		if err := errorTemplate.Execute(&b, p); err != nil {
			panic(err)
		}
		out[p.Code] = b.Bytes()
	}
	return out
}()
var errorCSP = func() string {
	sum := sha256.Sum256([]byte(errorCSS))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}()

// ErrorPage contains no request-derived text, infrastructure details, scripts,
// external assets or management links. Only gateway-generated errors use it.
func ErrorPage(w http.ResponseWriter, r *http.Request, status int) {
	body, ok := errorPages[status]
	if !ok {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", errorCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}
func proxyErrorStatus(err error) int {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
