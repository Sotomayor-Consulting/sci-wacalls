package main

import (
	"bytes"
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// uiAuthJS es el bootstrap que le enseña a la UI nativa de WaCalls a mandar la
// API key. Se inyecta en index.html al servirlo.
//
//go:embed uiauth.js
var uiAuthJS []byte

// staticHandler sirve el cliente estático. A index.html le inyecta uiauth.js
// para que sus peticiones lleven la API key; el resto de los archivos van tal
// cual. Sin esto, con auth activada la UI recibe 401 en todo y la única salida
// era apagar la autenticación del motor.
func (s *server) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.staticDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.servesIndex(r.URL.Path) {
			fs.ServeHTTP(w, r)
			return
		}
		html, err := os.ReadFile(filepath.Join(s.staticDir, "index.html"))
		if err != nil {
			fs.ServeHTTP(w, r)
			return
		}
		// El script va en <head> para correr ANTES del bundle de la app: si se
		// inyectara al final, la UI ya habría hecho sus primeras llamadas sin
		// cabecera.
		tag := []byte("<script>" + string(uiAuthJS) + "</script>")
		if i := bytes.Index(html, []byte("<head>")); i >= 0 {
			html = append(html[:i+len("<head>")], append(tag, html[i+len("<head>"):]...)...)
		} else {
			html = append(tag, html...)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(html)
	})
}

// servesIndex indica si la ruta debe responder con index.html (raíz o rutas del
// router del SPA, que no son archivos reales).
func (s *server) servesIndex(p string) bool {
	if p == "/" || p == "/index.html" {
		return true
	}
	if strings.Contains(filepath.Base(p), ".") {
		return false // parece un archivo (assets, worklets)
	}
	_, err := os.Stat(filepath.Join(s.staticDir, filepath.Clean(p)))
	return err != nil // no existe como archivo -> ruta del SPA
}
