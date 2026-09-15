/*
 * sci-wacalls — bootstrap de autenticación para la UI nativa de WaCalls.
 *
 * El cliente empaquetado de WaCalls no contempla API key: hace fetch/XHR sin
 * cabeceras, así que con WACALLS_API_KEY puesta recibe 401 y la UI queda
 * inservible (no se puede ni parear un número). La alternativa era apagar la
 * auth, lo que deja la API abierta a cualquiera que alcance el puerto.
 *
 * Este script se inyecta en index.html al servirlo y parchea fetch, XHR y
 * EventSource para adjuntar la clave, que se guarda en localStorage. Para SSE
 * usa ?apiKey= porque EventSource no puede setear cabeceras — el mismo camino
 * que ya acepta el middleware del servidor.
 */
(function () {
  "use strict";
  var KEY_NAME = "wacallsApiKey";
  var HEADER = "X-API-Key";

  function get() {
    try { return localStorage.getItem(KEY_NAME) || ""; } catch (_) { return ""; }
  }
  function set(v) {
    try { localStorage.setItem(KEY_NAME, v); } catch (_) {}
  }

  // Expuesto para poder cambiarla o borrarla desde la consola del navegador.
  window.wacallsSetApiKey = function (v) { set(v); location.reload(); };
  window.wacallsClearApiKey = function () {
    try { localStorage.removeItem(KEY_NAME); } catch (_) {}
    location.reload();
  };

  var origFetch = window.fetch;
  window.fetch = function (input, init) {
    var url = typeof input === "string" ? input : (input && input.url) || "";
    if (url.indexOf("/api/") !== -1) {
      var k = get();
      if (k) {
        init = init || {};
        var h = new Headers(init.headers || (typeof input !== "string" && input.headers) || {});
        if (!h.has(HEADER)) h.set(HEADER, k);
        init.headers = h;
      }
    }
    return origFetch.call(this, input, init);
  };

  var origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__wacallsApi = typeof url === "string" && url.indexOf("/api/") !== -1;
    return origOpen.apply(this, arguments);
  };
  var origSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function (body) {
    if (this.__wacallsApi) {
      var k = get();
      if (k) { try { this.setRequestHeader(HEADER, k); } catch (_) {} }
    }
    return origSend.call(this, body);
  };

  // EventSource no admite cabeceras: la clave va en la query.
  var OrigES = window.EventSource;
  if (OrigES) {
    window.EventSource = function (url, cfg) {
      var k = get();
      if (k && String(url).indexOf("/api/") !== -1 && String(url).indexOf("apiKey=") === -1) {
        url += (String(url).indexOf("?") === -1 ? "?" : "&") + "apiKey=" + encodeURIComponent(k);
      }
      return new OrigES(url, cfg);
    };
    window.EventSource.prototype = OrigES.prototype;
  }

  // Si no hay clave guardada, la pedimos antes de que arranque la app: sin ella
  // la UI solo mostraría errores 401 sin explicar por qué.
  if (!get()) {
    var k = window.prompt(
      "sci-wacalls — pegá la API key del motor (WACALLS_API_KEY).\n" +
      "Queda guardada en este navegador. Para cambiarla: wacallsSetApiKey('...')"
    );
    if (k) { set(k.trim()); }
  }
})();
