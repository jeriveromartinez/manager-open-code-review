// Package web expone la SPA de Vue embebida en el binario de qa.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist es el build de web/dist que copia `make web`. El prefijo all: incluye
// el .gitkeep para que el paquete compile antes del primer build de la web.
//
//go:embed all:dist
var dist embed.FS

const cacheImmutable = "public, max-age=31536000, immutable"

// Handler sirve la SPA embebida: archivos existentes tal cual, index.html para
// rutas sin extensión (navegación del router) y 404 para rutas con extensión
// que no existen. Los archivos bajo /assets/ se sirven con caché inmutable.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return handlerFS(sub)
}

func handlerFS(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		name := strings.TrimPrefix(clean, "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", cacheImmutable)
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			// Una ruta con extensión es un archivo: no debe caer al index.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			body, err = fs.ReadFile(fsys, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}

		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
}
