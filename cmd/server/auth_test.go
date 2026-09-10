package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// reqStatus corre una petición por el handler completo (CORS + auth + mux).
func reqStatus(h http.Handler, method, target string, key string) int {
	r := httptest.NewRequest(method, target, nil)
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestAuthDisabledByDefault(t *testing.T) {
	srv := newTestServer(t) // sin claves
	h := srv.routes()
	if code := reqStatus(h, http.MethodGet, "/api/sessions", ""); code != http.StatusOK {
		t.Fatalf("sin auth, /api/sessions debería dar 200, got %d", code)
	}
}

func TestAuthMasterKey(t *testing.T) {
	srv := newTestServer(t)
	srv.apiKey = "master"
	h := srv.routes()

	if code := reqStatus(h, http.MethodGet, "/api/sessions", ""); code != http.StatusUnauthorized {
		t.Fatalf("sin clave -> 401, got %d", code)
	}
	if code := reqStatus(h, http.MethodGet, "/api/sessions", "master"); code != http.StatusOK {
		t.Fatalf("clave maestra -> 200, got %d", code)
	}
	if code := reqStatus(h, http.MethodGet, "/api/sessions", "otra"); code != http.StatusUnauthorized {
		t.Fatalf("clave inválida -> 401, got %d", code)
	}
}

func TestAuthWidgetKeyScope(t *testing.T) {
	srv := newTestServer(t)
	srv.apiKey = "master"
	srv.widgetKey = "widget"
	h := srv.routes()

	// Ruta permitida al widget (resolve): la clave de widget pasa auth (400 por
	// faltar params, NO 401).
	if code := reqStatus(h, http.MethodGet, "/api/chatwoot/resolve", "widget"); code != http.StatusBadRequest {
		t.Fatalf("resolve con widget key debería pasar auth (400), got %d", code)
	}
	// Ruta de llamada (permitida): pasa auth (404 no such session, NO 401).
	if code := reqStatus(h, http.MethodPost, "/api/sessions/x/calls", "widget"); code != http.StatusNotFound {
		t.Fatalf("calls con widget key debería pasar auth (404), got %d", code)
	}
	// Ruta NO permitida al widget (listar sesiones): 401.
	if code := reqStatus(h, http.MethodGet, "/api/sessions", "widget"); code != http.StatusUnauthorized {
		t.Fatalf("listar sesiones con widget key -> 401, got %d", code)
	}
	// Borrar sesión (gestión) con widget key: 401.
	if code := reqStatus(h, http.MethodDelete, "/api/sessions/x", "widget"); code != http.StatusUnauthorized {
		t.Fatalf("delete sesión con widget key -> 401, got %d", code)
	}
}

func TestAuthPublicRoutes(t *testing.T) {
	srv := newTestServer(t)
	srv.apiKey = "master"
	h := srv.routes()

	// widget.js es público aunque haya auth.
	if code := reqStatus(h, http.MethodGet, "/widget.js", ""); code != http.StatusOK {
		t.Fatalf("widget.js debería ser público (200), got %d", code)
	}
	// El webhook de Chatwoot queda exento (Chatwoot no puede mandar la cabecera):
	// pasa auth y da 404 por sesión inexistente, no 401.
	if code := reqStatus(h, http.MethodPost, "/api/sessions/x/chatwoot/webhook", ""); code != http.StatusNotFound {
		t.Fatalf("webhook exento debería pasar auth (404), got %d", code)
	}
}

func TestAuthQueryParamKey(t *testing.T) {
	srv := newTestServer(t)
	srv.apiKey = "master"
	h := srv.routes()
	// EventSource no puede setear headers: se acepta ?apiKey= .
	if code := reqStatus(h, http.MethodGet, "/api/chatwoot/resolve?apiKey=master", ""); code != http.StatusBadRequest {
		t.Fatalf("apiKey por query debería pasar auth (400 por faltar params), got %d", code)
	}
}
