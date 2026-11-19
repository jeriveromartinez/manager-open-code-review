package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPAFallback(t *testing.T) {
	const indexHTML = "<html><body>QA Control Center</body></html>\n"
	const assetJS = "console.log('qa');\n"

	h := handlerFS(fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte(indexHTML)},
		"assets/a.js": &fstest.MapFile{Data: []byte(assetJS)},
	})

	t.Run("asset existente con cabecera immutable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/a.js", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q, want %q", got, "public, max-age=31536000, immutable")
		}
		body, err := io.ReadAll(rec.Result().Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != assetJS {
			t.Errorf("body = %q, want %q", body, assetJS)
		}
	})

	t.Run("ruta sin extension inexistente sirve index.html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teams", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		body, err := io.ReadAll(rec.Result().Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != indexHTML {
			t.Errorf("body = %q, want index.html content %q", body, indexHTML)
		}
	})

	t.Run("ruta inexistente con extension da 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}
