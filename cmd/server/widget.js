/*
 * sci-wacalls — widget de llamada para Chatwoot.
 *
 * Se inyecta en Chatwoot vía Super Admin > App Configs > DASHBOARD_SCRIPTS:
 *   <script src="https://TU-WACALLS/widget.js" data-url="https://TU-WACALLS"></script>
 *
 * Agrega un botón de teléfono en la conversación; al hacer clic resuelve la
 * sesión de WhatsApp y el contacto (GET /api/chatwoot/resolve), inicia la
 * llamada y transporta el audio del micrófono por un data channel WebRTC
 * ("pcm", Int16LE a 16 kHz) contra la API de wacalls — el mismo contrato que
 * usa el cliente nativo.
 *
 * Código original de sci-wacalls. Sin dependencias externas.
 */
(function () {
  "use strict";

  var script = document.currentScript;
  var BASE = (script && (script.getAttribute("data-url") || new URL(script.src).origin)) || "";
  // Clave ACOTADA de widget (WACALLS_WIDGET_KEY), no la maestra: queda visible en
  // el DOM, así que solo debe autorizar resolver contacto y operar llamadas.
  var KEY = (script && script.getAttribute("data-api-key")) || "";
  var SAMPLE_RATE = 16000;
  var PCM_LABEL = "pcm";

  // Identidad estable de ESTE navegador. El motor la usa para saber quién se
  // queda con una llamada entrante (setOwner): sin ella todos los agentes son
  // el mismo cliente ("") y dos personas pueden contestar la misma llamada,
  // negociando ambas su WebRTC contra ella.
  var CLIENT_ID = (function () {
    try {
      var k = localStorage.getItem("wacallsClientId");
      if (!k) {
        k = (window.crypto && crypto.randomUUID)
          ? crypto.randomUUID()
          : "cw-" + Date.now() + "-" + Math.random().toString(36).slice(2, 10);
        localStorage.setItem("wacallsClientId", k);
      }
      return k;
    } catch (_) {
      return "cw-" + Math.random().toString(36).slice(2, 10);
    }
  })();

  function authHeaders(extra) {
    var h = extra || {};
    if (KEY) h["X-API-Key"] = KEY;
    h["X-Client-Id"] = CLIENT_ID;
    return h;
  }

  // ---------- helpers PCM ----------
  function float32ToInt16LE(f32) {
    var view = new DataView(new ArrayBuffer(f32.length * 2));
    for (var i = 0; i < f32.length; i++) {
      var s = f32[i];
      if (isNaN(s)) s = 0;
      else if (s > 1) s = 1;
      else if (s < -1) s = -1;
      view.setInt16(i * 2, s < 0 ? Math.round(s * 32768) : Math.round(s * 32767), true);
    }
    return view.buffer;
  }
  function int16LEToFloat32(buf) {
    var view = new DataView(buf);
    var n = Math.floor(buf.byteLength / 2);
    var out = new Float32Array(n);
    for (var i = 0; i < n; i++) out[i] = view.getInt16(i * 2, true) / 32768;
    return out;
  }

  // ---------- worklets (inline, como blob URLs) ----------
  var CAPTURE_SRC =
    'class CaptureProcessor extends AudioWorkletProcessor{' +
    'process(inputs){var c=inputs[0]&&inputs[0][0];if(c&&c.length)this.port.postMessage(c.slice(0));return true;}}' +
    'registerProcessor("capture-processor",CaptureProcessor);';
  var PLAYBACK_SRC =
    'const RING=16000*2;class PlaybackProcessor extends AudioWorkletProcessor{' +
    'constructor(){super();this.ring=new Float32Array(RING);this.read=0;this.write=0;this.available=0;' +
    'this.port.onmessage=(e)=>{var d=e.data;for(var i=0;i<d.length;i++){this.ring[this.write]=d[i];' +
    'this.write=(this.write+1)%RING;if(this.available<RING)this.available++;else this.read=(this.read+1)%RING;}};}' +
    'process(_in,outputs){var o=outputs[0][0];if(!o)return true;for(var i=0;i<o.length;i++){' +
    'if(this.available>0){o[i]=this.ring[this.read];this.read=(this.read+1)%RING;this.available--;}else o[i]=0;}return true;}}' +
    'registerProcessor("playback-processor",PlaybackProcessor);';
  function blobURL(src) {
    return URL.createObjectURL(new Blob([src], { type: "application/javascript" }));
  }

  // ---------- API ----------
  function apiGet(path) {
    return fetch(BASE + path, { headers: authHeaders({ "Content-Type": "application/json" }) }).then(function (r) {
      if (!r.ok) return r.json().then(function (e) { throw new Error(e.error || r.status); });
      return r.json();
    });
  }
  function apiPost(path, body) {
    return fetch(BASE + path, {
      method: "POST",
      headers: authHeaders({ "Content-Type": "application/json" }),
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      if (!r.ok) return r.json().then(function (e) { throw new Error(e.error || r.status); });
      return r.json();
    });
  }
  function apiDelete(path) {
    return fetch(BASE + path, { method: "DELETE", headers: authHeaders() });
  }

  // account_id + conversation_id desde la URL de Chatwoot
  // (…/accounts/{aid}/…/conversations/{cid}).
  function currentContext() {
    var m = location.pathname.match(/accounts\/(\d+).*conversations\/(\d+)/);
    if (!m) return null;
    return { accountId: m[1], conversationId: m[2] };
  }

  // ---------- llamada WebRTC ----------
  function startWebRTCCall(sid, callId, onState, onClose) {
    var pc, ctx, micStream, audioEl;
    var closed = false;

    function cleanup() {
      if (closed) return;
      closed = true;
      try { micStream && micStream.getTracks().forEach(function (t) { t.stop(); }); } catch (e) {}
      try { ctx && ctx.close(); } catch (e) {}
      try { pc && pc.close(); } catch (e) {}
      try { audioEl && audioEl.remove(); } catch (e) {}
      onClose && onClose();
    }

    navigator.mediaDevices.getUserMedia({ audio: true })
      .then(function (stream) {
        micStream = stream;
        pc = new RTCPeerConnection({ iceServers: [] });
        pc.oniceconnectionstatechange = function () {
          if (pc.iceConnectionState === "connected") onState && onState("connected");
          if (pc.iceConnectionState === "failed" || pc.iceConnectionState === "closed") cleanup();
        };
        var dc = pc.createDataChannel(PCM_LABEL, { ordered: true });
        dc.binaryType = "arraybuffer";

        ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
        return Promise.all([
          ctx.audioWorklet.addModule(blobURL(CAPTURE_SRC)),
          ctx.audioWorklet.addModule(blobURL(PLAYBACK_SRC)),
        ]).then(function () {
          return ctx.resume();
        }).then(function () {
          var micSource = ctx.createMediaStreamSource(micStream);
          var capture = new AudioWorkletNode(ctx, "capture-processor");
          capture.port.onmessage = function (e) {
            if (dc.readyState === "open") dc.send(float32ToInt16LE(e.data));
          };
          micSource.connect(capture);
          // Necesario para que el grafo "tire" del worklet de captura; no hay
          // eco porque el processor nunca escribe en sus salidas.
          capture.connect(ctx.destination);

          // El nodo de reproducción NO tiene entradas: se alimenta por
          // postMessage. Hay que declararlo explícitamente — con el default
          // (numberOfInputs:1) el conteo de canales de salida se deriva de la
          // entrada, que al no estar conectada es 0, así que outputs[0][0]
          // queda undefined y el worklet emite silencio para siempre.
          var playback = new AudioWorkletNode(ctx, "playback-processor", {
            numberOfInputs: 0,
            numberOfOutputs: 1,
            outputChannelCount: [1],
          });
          dc.onmessage = function (e) { playback.port.postMessage(int16LEToFloat32(e.data)); };
          // Directo a los altavoces: un MediaStreamDestination + <audio> añade
          // un remuestreo (el contexto va a 16 kHz) y la política de autoplay,
          // dos formas silenciosas de perder el audio del contacto.
          playback.connect(ctx.destination);

          return pc.createOffer();
        });
      })
      .then(function (offer) {
        return pc.setLocalDescription(offer);
      })
      .then(function () {
        return new Promise(function (resolve) {
          if (pc.iceGatheringState === "complete") return resolve();
          pc.addEventListener("icegatheringstatechange", function () {
            if (pc.iceGatheringState === "complete") resolve();
          });
        });
      })
      .then(function () {
        return apiPost("/api/sessions/" + sid + "/calls/" + callId + "/webrtc", {
          sdp_offer: pc.localDescription.sdp,
        });
      })
      .then(function (res) {
        return pc.setRemoteDescription({ type: "answer", sdp: res.sdp_answer });
      })
      .catch(function (err) {
        onState && onState("error", err.message);
        cleanup();
      });

    return {
      hangup: function () {
        apiDelete("/api/sessions/" + sid + "/calls/" + callId);
        cleanup();
      },
      // Silencia el micrófono sin cortar la captura: el worklet sigue leyendo,
      // pero las pistas deshabilitadas entregan silencio.
      setMuted: function (muted) {
        if (!micStream) return false;
        micStream.getAudioTracks().forEach(function (t) { t.enabled = !muted; });
        return true;
      },
    };
  }

  // ---------- UI ----------
  var panel, activeCall, durTimer;
  // Llamada entrante pendiente de contestar: {sessionId, callId, label}.
  var incoming = null;
  // ID de la llamada en curso, para filtrar los eventos del SSE compartido.
  var currentCallId = null;
  var currentSessionId = null;
  // true cuando la llamada venía de antes de recargar la página: la señalización
  // sigue viva en el motor, pero el audio del navegador se perdió.
  var recovered = false;
  // true entre que se pide la llamada y que el POST devuelve su id. En esa
  // ventana ya llega el call-list con la llamada adentro, y sin este flag el
  // camino de recuperación la confunde con una huérfana de otra pestaña.
  var starting = false;

  function startDurationTimer(startedAt) {
    stopDurationTimer();
    var start = startedAt || Date.now();
    function tick() {
      var s = Math.floor((Date.now() - start) / 1000);
      var mm = String(Math.floor(s / 60)).padStart(2, "0");
      var ss = String(s % 60).padStart(2, "0");
      setStatus("En llamada · " + mm + ":" + ss);
    }
    tick();
    durTimer = setInterval(tick, 1000);
  }
  function stopDurationTimer() {
    if (durTimer) { clearInterval(durTimer); durTimer = null; }
  }

  // ---------- timbre de llamada entrante ----------
  // Pitidos sintetizados con un oscilador: no hay que empaquetar ni servir un
  // archivo de audio, y el contexto se cierra al dejar de sonar.
  var ringCtx = null, ringTimer = null;
  function playRing() {
    stopRing();
    try {
      var AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return;
      ringCtx = new AC();
      var beep = function () {
        if (!ringCtx) return;
        var o = ringCtx.createOscillator(), g = ringCtx.createGain();
        o.type = "sine";
        o.frequency.value = 480;
        o.connect(g);
        g.connect(ringCtx.destination);
        var t = ringCtx.currentTime;
        g.gain.setValueAtTime(0.0001, t);
        g.gain.exponentialRampToValueAtTime(0.18, t + 0.05);
        g.gain.exponentialRampToValueAtTime(0.0001, t + 0.9);
        o.start(t);
        o.stop(t + 0.95);
      };
      beep();
      ringTimer = setInterval(beep, 2500);
    } catch (_) {}
  }
  function stopRing() {
    if (ringTimer) { clearInterval(ringTimer); ringTimer = null; }
    if (ringCtx) { try { ringCtx.close(); } catch (_) {} ringCtx = null; }
  }

  var BTN_CSS = "flex:1;padding:8px;border:0;border-radius:8px;color:#fff;font-weight:600;cursor:pointer;";

  // panelShell dibuja el panel flotante (abajo a la derecha) con un título, una
  // línea de estado y los botones que correspondan al momento de la llamada.
  // ensurePanelAttached vuelve a colgar el panel del body si lo sacaron de ahí.
  // Chatwoot es un SPA: al navegar puede barrer nodos que no son suyos, y
  // conservar la referencia en la variable no alcanza — seguiríamos escribiendo
  // el estado y el cronómetro en un nodo DESCONECTADO, invisible, mientras la
  // llamada sigue en curso.
  function ensurePanelAttached() {
    if (panel && !panel.isConnected) document.body.appendChild(panel);
  }

  function panelShell(title, status, buttonsHTML) {
    if (!panel) {
      panel = document.createElement("div");
      panel.style.cssText =
        "position:fixed;right:20px;bottom:20px;z-index:99999;background:#fff;border:1px solid #e0e0e0;" +
        "border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,.15);padding:16px;min-width:220px;" +
        "font-family:system-ui,sans-serif;font-size:14px;color:#1f2937;";
      document.body.appendChild(panel);
    }
    ensurePanelAttached();
    panel.hidden = false;
    panel.innerHTML =
      '<div style="font-weight:600;margin-bottom:4px">' + escapeHTML(title) + "</div>" +
      '<div id="wc-status" style="color:#6b7280;margin-bottom:12px">' + escapeHTML(status) + "</div>" +
      '<div style="display:flex;gap:8px">' + buttonsHTML + "</div>";
  }

  var muted = false;

  function showPanel(name) {
    panelShell(name, "Llamando…",
      '<button id="wc-mute" style="' + BTN_CSS + 'background:#6b7280">Silenciar</button>' +
      '<button id="wc-hangup" style="' + BTN_CSS + 'background:#ef4444">Colgar</button>');
    panel.querySelector("#wc-hangup").onclick = endCall;
    var mb = panel.querySelector("#wc-mute");
    mb.onclick = function () {
      if (!activeCall || !activeCall.setMuted) return;
      muted = !muted;
      if (!activeCall.setMuted(muted)) { muted = !muted; return; }
      mb.textContent = muted ? "Reactivar" : "Silenciar";
      mb.style.background = muted ? "#e5484d" : "#6b7280";
    };
  }

  function showIncomingPanel(label) {
    panelShell(label, "Llamada entrante…",
      '<button id="wc-answer" style="' + BTN_CSS + 'background:#22c55e">Contestar</button>' +
      '<button id="wc-reject" style="' + BTN_CSS + 'background:#ef4444">Rechazar</button>');
    panel.querySelector("#wc-answer").onclick = acceptIncoming;
    panel.querySelector("#wc-reject").onclick = rejectIncoming;
  }

  function setStatus(txt) {
    var el = panel && panel.querySelector("#wc-status");
    if (el) el.textContent = txt;
  }
  function hidePanel() {
    if (panel) panel.hidden = true;
  }

  // wireCallMedia arma el camino de audio del navegador para una llamada ya
  // existente (saliente recién creada o entrante ya aceptada).
  function wireCallMedia(sessionId, callId) {
    currentCallId = callId;
    currentSessionId = sessionId;
    recovered = false;
    activeCall = startWebRTCCall(
      sessionId,
      callId,
      function (state, msg) {
        if (state === "error") setStatus("Error: " + (msg || ""));
      },
      function () { activeCall = null; stopDurationTimer(); }
    );
  }

  function beginCall() {
    var cx = currentContext();
    if (!cx) return;
    showPanel("Llamada");
    setStatus("Resolviendo contacto…");
    starting = true; // ver la nota en la declaración: evita auto-recuperarnos
    // Ya resuelto al abrir la conversación: evita una segunda vuelta.
    var pre = resolved && resolved.phone ? Promise.resolve(resolved)
      : apiGet("/api/chatwoot/resolve?account_id=" + cx.accountId + "&conversation_id=" + cx.conversationId);
    pre
      .then(function (r) {
        showPanel(r.name || r.phone);
        setStatus("Llamando…");
        return apiPost("/api/sessions/" + r.session_id + "/calls", { phone: r.phone }).then(function (c) {
          // WebRTC solo arma el camino de audio navegador↔servidor (ICE). NO
          // arranca el timer: el audio del cliente recién fluye cuando CONTESTA.
          // El estado REAL (suena / contestó / colgó) llega por SSE.
          wireCallMedia(r.session_id, c.call.callId);
          starting = false; // ya tenemos el id: el filtro normal alcanza
        });
      })
      .catch(function (err) {
        starting = false;
        setStatus("No se pudo llamar: " + err.message);
        setTimeout(hidePanel, 4000);
      });
  }

  // acceptIncoming contesta la llamada entrante: avisa a WhatsApp por /accept y
  // recién entonces arma el audio. Nosotros contestamos, así que el contador
  // arranca aquí sin esperar el SSE.
  function acceptIncoming() {
    var inc = incoming;
    if (!inc) return;
    incoming = null;
    stopRing();
    showPanel(inc.label);
    setStatus("Conectando…");
    apiPost("/api/sessions/" + inc.sessionId + "/calls/" + inc.callId + "/accept", {})
      .then(function () {
        wireCallMedia(inc.sessionId, inc.callId);
        startDurationTimer();
      })
      .catch(function (err) {
        // 409 del motor = otro agente la reclamó primero. No hay que colgar la
        // llamada en ese caso: la está atendiendo alguien más.
        var msg = String(err.message || "");
        if (msg.indexOf("claimed") !== -1 || msg.indexOf("already on a call") !== -1) {
          setStatus("La atendió otro agente");
        } else {
          setStatus("No se pudo contestar: " + msg);
          apiDelete("/api/sessions/" + inc.sessionId + "/calls/" + inc.callId);
        }
        setTimeout(hidePanel, 4000);
      });
  }

  function rejectIncoming() {
    var inc = incoming;
    incoming = null;
    stopRing();
    hidePanel();
    if (inc) {
      apiPost("/api/sessions/" + inc.sessionId + "/calls/" + inc.callId + "/reject", {})
        .catch(function () {});
    }
  }

  // Motivo con el que el backend cerró la llamada (evento call-ended, campo
  // "reason") — viajaba hasta acá y se descartaba sin mostrar nada: el panel
  // se cerraba en silencio y el agente no se enteraba de si no contestó, si
  // rechazó, o si se cortó por otra razón.
  var END_REASONS = {
    timeout: "No contestó", busy: "Ocupado", declined: "Rechazada",
    cancelled: "Cancelada", do_not_disturb: "No molestar",
    failed: "Falló la llamada",
  };

  // endCall recibe reason SOLO cuando el cierre vino del backend (SSE); un
  // colgado propio (botón "Colgar") no manda nada, porque el agente ya sabe
  // que colgó y no hace falta mostrarle un cartel.
  function endCall(reason) {
    if (activeCall) activeCall.hangup();
    // Tras recargar la página no hay activeCall que colgar, pero la llamada
    // sigue viva en el motor: hay que terminarla por API o queda colgada.
    else if (currentCallId && currentSessionId) {
      apiDelete("/api/sessions/" + currentSessionId + "/calls/" + currentCallId);
    }
    activeCall = null;
    currentCallId = null;
    currentSessionId = null;
    recovered = false;
    starting = false; // si se cortó a mitad del arranque, no dejarlo trabado
    muted = false;
    stopDurationTimer();
    stopRing();
    if (reason && END_REASONS[reason]) {
      setStatus(END_REASONS[reason]);
      setTimeout(hidePanel, 3000);
    } else {
      hidePanel();
    }
  }

  // ---------- eventos por SSE ----------
  // Un solo EventSource permanente: además del estado de la llamada en curso,
  // es lo que nos entera de las llamadas ENTRANTES, que pueden llegar en
  // cualquier momento y no solo mientras hay una llamada abierta.
  var es = null;

  function connectEvents() {
    if (es && es.readyState !== 2) return; // 2 = CLOSED
    try {
      var url = BASE + "/api/events?clientId=" + encodeURIComponent(CLIENT_ID) +
        (KEY ? "&apiKey=" + encodeURIComponent(KEY) : "");
      es = new EventSource(url);
      es.onmessage = function (e) {
        var m;
        try { m = JSON.parse(e.data); } catch (_) { return; }
        if (m) onEvent(m);
      };
      es.onerror = function () { /* EventSource reintenta solo */ };
    } catch (_) {}
  }

  // applyStatus refleja en el panel el estado que reporta el backend.
  function applyStatus(status) {
    if (recovered) return; // el panel ya dice que el audio se perdió
    if (status === "connected") {
      stopRing(); // el cliente CONTESTÓ: se acaba el tono de espera
      if (!durTimer) startDurationTimer();
    } else if (status === "ringing") {
      if (!durTimer) {
        setStatus("Sonando…");
        // Tono de espera para quien llama, como en una llamada de teléfono
        // normal. Antes solo sonaba en la llamada ENTRANTE (playRing() tenía
        // un único call site) — acá faltaba conectarlo. endCall() ya limpia
        // con stopRing() sin condición, cubre colgar/rechazo/sin respuesta.
        if (!ringCtx) playRing();
      }
    }
  }

  function onEvent(m) {
    var ended = m.type === "call-ended" || (m.type === "call-status" && m.status === "ended");

    // La lista completa llega con cada cambio de estado y nos sirve de red: el
    // backend emite el call-status de una llamada saliente ANTES de responder
    // el POST que nos da su id, así que la transición a "connected" puede
    // ocurrir antes de que sepamos qué id filtrar.
    if (m.type === "call-list") {
      if (!m.calls) return;
      // Sin llamada local pero con una activa en el motor: la página se
      // recargó en medio de la llamada. Mostramos el panel para que el agente
      // pueda al menos colgarla, y avisamos que el audio se perdió — dejarla
      // "en curso" sin más sería mentirle.
      // `starting` excluye la llamada que ESTE navegador acaba de pedir: el
      // call-list con ella ya adentro llega antes de que responda el POST, así
      // que sin este guard la tomábamos por huérfana y la "recuperábamos"
      // —contador arrancando en pleno timbrado, panel diciendo que se recargó
      // la página, y recovered=true dejando applyStatus() muerto para siempre.
      if (!currentCallId && !incoming && !starting) {
        for (var j = 0; j < m.calls.length; j++) {
          var c = m.calls[j];
          if (c.status === "connected" || c.status === "ringing") {
            currentCallId = c.callId;
            currentSessionId = c.sessionId;
            recovered = true;
            panelShell("Llamada en curso",
              "Sin audio: se recargó la página",
              '<button id="wc-hangup" style="' + BTN_CSS + 'background:#ef4444">Colgar</button>');
            panel.querySelector("#wc-hangup").onclick = endCall;
            // Solo se cuenta lo CONECTADO. Una recuperada que todavía timbra
            // no tiene duración que mostrar — mismo criterio que AstraCalls,
            // que centraliza el guard en formatCallDuration().
            if (c.status === "connected" && c.startedAt) startDurationTimer(c.startedAt);
            return;
          }
        }
        return;
      }
      if (!currentCallId) return;
      for (var i = 0; i < m.calls.length; i++) {
        if (m.calls[i].callId === currentCallId) {
          applyStatus(m.calls[i].status);
          return;
        }
      }
      return;
    }

    // Llamada entrante nueva → timbre + panel para contestar/rechazar.
    if (m.type === "incoming") {
      if (activeCall || currentCallId) return;          // ya estamos en llamada
      if (incoming && incoming.callId === m.id) return; // ya está sonando esta
      var who = m.phone || m.peer || "desconocido";
      incoming = {
        sessionId: m.sessionId,
        callId: m.id,
        label: m.name ? m.name + " · " + who : who,
      };
      showIncomingPanel(incoming.label);
      playRing();
      return;
    }

    // Otro agente se quedó con esta entrante: hay que dejar de sonar, o todos
    // los navegadores siguen timbrando por una llamada que ya está atendida.
    if (m.type === "incoming-claimed" && incoming && m.id === incoming.callId) {
      if (m.owner !== CLIENT_ID) {
        incoming = null;
        stopRing();
        setStatus("Atendida por otro agente");
        setTimeout(hidePanel, 3000);
      }
      return;
    }

    // La entrante se cortó antes de contestar (el cliente desistió).
    if (incoming && m.id === incoming.callId && ended) {
      incoming = null;
      stopRing();
      hidePanel();
      return;
    }

    if (!currentCallId || m.id !== currentCallId) return;
    if (ended) {
      endCall(m.reason);
      return;
    }
    if (m.type === "call-status") applyStatus(m.status);
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  // ---------- inyección del botón ----------
  var BTN_ID = "wacalls-call-btn";
  var PHONE_SVG =
    '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" ' +
    'stroke-linecap="round" stroke-linejoin="round"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 ' +
    '19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72c.13.96.36 1.9.7 2.81a2 2 0 0 1-.45 ' +
    '2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45c.91.34 1.85.57 2.81.7A2 2 0 0 1 22 16.92z"/></svg>';

  // ---------- ¿esta conversación admite llamada? ----------
  // El botón solo se inyecta si el contacto tiene teléfono. Un GRUPO no lo
  // tiene: su "contacto" en Chatwoot es el grupo mismo, así que llamarlo no
  // tiene sentido y antes el agente se enteraba recién al hacer clic, con un
  // error. Se resuelve al cambiar de conversación, no en cada mutación del DOM.
  var callable = false;
  var boundKey = null;
  var resolved = null;

  function convKey() {
    var cx = currentContext();
    return cx ? cx.accountId + "/" + cx.conversationId : null;
  }

  function refreshBinding() {
    var key = convKey();
    if (key === boundKey) return;
    boundKey = key;
    callable = false;
    resolved = null;
    removeButton();
    if (!key) return;
    var parts = key.split("/");
    apiGet("/api/chatwoot/resolve?account_id=" + parts[0] + "&conversation_id=" + parts[1])
      .then(function (r) {
        if (convKey() !== key) return; // el agente ya cambió de conversación
        resolved = r;
        callable = !!(r && r.phone);
        ensureButton();
      })
      .catch(function () {
        // 422 (grupo o contacto sin teléfono), 404, o sin sesión: no es llamable.
        if (convKey() === key) callable = false;
      });
  }

  function removeButton() {
    var old = document.getElementById(BTN_ID);
    if (old) old.remove();
  }

  // Selector opcional del contenedor donde anclar el botón, por si el DOM de
  // Chatwoot cambia: <script ... data-anchor=".conversation--header .actions">.
  var ANCHOR = (script && script.getAttribute("data-anchor")) || "";

  // findActionsContainer localiza la barra de acciones del header de la
  // conversación (donde viven "resolver", "más opciones", etc.) para que el
  // botón de llamada quede junto a los nativos en vez de flotando encima del
  // editor. Heurística: un botón cuadrado de ~32px en el tercio derecho de la
  // ventana cuyo padre agrupa entre 2 y 6 botones — esa firma es la barra de
  // acciones. Devuelve también un hermano del que copiar las clases nativas.
  function findActionsContainer() {
    if (ANCHOR) {
      var a = document.querySelector(ANCHOR);
      if (a) return { container: a, sibling: a.querySelector("button") };
    }
    var btns = document.querySelectorAll("header button, .conversation--header button");
    if (!btns.length) btns = document.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      var b = btns[i];
      if (b.id === BTN_ID) continue;
      var r = b.getBoundingClientRect();
      var square = r.width >= 26 && r.width <= 44 && r.height >= 26 && r.height <= 44;
      if (!square || r.left < window.innerWidth * 0.55 || r.top > window.innerHeight * 0.4) continue;
      var p = b.parentElement;
      if (!p) continue;
      var group = p.querySelectorAll(":scope > button");
      if (group.length >= 2 && group.length <= 6) return { container: p, sibling: b };
    }
    return null;
  }

  var anchorTries = 0;

  function ensureButton() {
    // Solo en conversaciones que admitan llamada (contacto con teléfono).
    if (!currentContext() || !callable) {
      removeButton();
      anchorTries = 0;
      return;
    }
    if (document.getElementById(BTN_ID)) return;

    var found = findActionsContainer();
    // El header de Chatwoot se monta después del primer render; damos margen
    // antes de caer al botón flotante, para no dejar la llamada inaccesible.
    if (!found && ++anchorTries < 25) return;
    if (found && found.container.querySelector("#" + BTN_ID)) return;

    var btn = document.createElement("button");
    btn.id = BTN_ID;
    btn.type = "button";
    btn.title = "Llamar por WhatsApp";
    btn.innerHTML = PHONE_SVG;
    btn.onclick = function (e) {
      e.preventDefault();
      e.stopPropagation();
      beginCall();
    };

    if (found) {
      // Hereda las clases del botón vecino para verse nativo en cualquier tema.
      btn.className = (found.sibling && found.sibling.className) ||
        "inline-flex items-center justify-center h-8 w-8 p-0 rounded-lg";
      btn.style.cssText = "color:#25D366;cursor:pointer;";
      found.container.appendChild(btn);
    } else {
      btn.style.cssText =
        "position:fixed;right:20px;bottom:80px;z-index:99998;width:48px;height:48px;border:0;border-radius:50%;" +
        "background:#25D366;color:#fff;box-shadow:0 4px 12px rgba(0,0,0,.2);cursor:pointer;display:flex;" +
        "align-items:center;justify-content:center;";
      document.body.appendChild(btn);
    }
  }

  var obs = new MutationObserver(function () {
    refreshBinding();
    ensureButton();
    // Si la navegación del SPA se llevó el panel durante una llamada, lo
    // devolvemos en el mismo tick en que se lo llevaron.
    if (activeCall || incoming || durTimer) ensurePanelAttached();
  });
  obs.observe(document.body, { childList: true, subtree: true });
  // La URL cambia al saltar de conversación sin que el DOM mute siempre, y el
  // header puede remontarse: reintentamos un rato tras la carga.
  (function retry(n) {
    refreshBinding();
    ensureButton();
    if (n < 40) setTimeout(function () { retry(n + 1); }, 800);
  })(0);
  // La URL cambia al saltar de conversación sin que el DOM mute siempre.
  setInterval(refreshBinding, 1000);

  // El SSE queda conectado siempre, no solo durante una llamada: es el canal
  // por el que llegan las llamadas ENTRANTES. El chequeo periódico lo revive si
  // el navegador lo dejó cerrado (pestaña suspendida, red caída).
  connectEvents();
  setInterval(connectEvents, 5000);
  console.log("[sci-wacalls] widget cargado. base=" + BASE);
})();
