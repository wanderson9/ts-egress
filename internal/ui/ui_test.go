package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedPages(t *testing.T) {
	for _, page := range []string{"setup", "dashboard"} {
		w := httptest.NewRecorder()
		Serve(w, httptest.NewRequest("GET", "/", nil), page)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="`+page+`"`) || strings.Contains(w.Body.String(), "__PAGE__") {
			t.Fatalf("bad %s entrypoint: %s", page, w.Body.String())
		}
	}
	for _, path := range []string{"/app.js", "/app.css"} {
		w := httptest.NewRecorder()
		Serve(w, httptest.NewRequest("GET", path, nil), "setup")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("missing asset %s", path)
		}
	}
	w := httptest.NewRecorder()
	Serve(w, httptest.NewRequest("GET", "/missing.js", nil), "setup")
	if w.Code != 404 {
		t.Fatal("unknown asset accepted")
	}
}
