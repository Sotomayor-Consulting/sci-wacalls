package main

import (
	_ "embed"
	"net/http"
)

// widgetJS es el widget de llamada para Chatwoot, embebido en el binario para
// servirlo sin depender del build del cliente.
//
//go:embed widget.js
var widgetJS []byte

// handleWidgetJS sirve widget.js. Lo carga el navegador del agente desde el
// origen de wacalls (CORS abierto por withCORS).
func (s *server) handleWidgetJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(widgetJS)
}
