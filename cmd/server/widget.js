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

  function authHeaders(extra) {
    var h = extra || {};
    if (KEY) h["X-API-Key"] = KEY;
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
          capture.connect(ctx.destination);

          var playback = new AudioWorkletNode(ctx, "playback-processor");
          var dest = ctx.createMediaStreamDestination();
          playback.connect(dest);
          dc.onmessage = function (e) { playback.port.postMessage(int16LEToFloat32(e.data)); };

          // reproducir el audio del contacto
          audioEl = document.createElement("audio");
          audioEl.autoplay = true;
          audioEl.srcObject = dest.stream;
          document.body.appendChild(audioEl);

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
    };
  }

  // ---------- UI ----------
  var panel, activeCall, durTimer;

  function startDurationTimer() {
    stopDurationTimer();
    var start = Date.now();
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

  function showPanel(name) {
    if (!panel) {
      panel = document.createElement("div");
      panel.style.cssText =
        "position:fixed;right:20px;bottom:20px;z-index:99999;background:#fff;border:1px solid #e0e0e0;" +
        "border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,.15);padding:16px;min-width:220px;" +
        "font-family:system-ui,sans-serif;font-size:14px;color:#1f2937;";
      document.body.appendChild(panel);
    }
    panel.hidden = false;
    panel.innerHTML =
      '<div style="font-weight:600;margin-bottom:4px">' + escapeHTML(name) + "</div>" +
      '<div id="wc-status" style="color:#6b7280;margin-bottom:12px">Llamando…</div>' +
      '<button id="wc-hangup" style="width:100%;padding:8px;border:0;border-radius:8px;background:#ef4444;' +
      'color:#fff;font-weight:600;cursor:pointer">Colgar</button>';
    panel.querySelector("#wc-hangup").onclick = endCall;
  }
  function setStatus(txt) {
    var el = panel && panel.querySelector("#wc-status");
    if (el) el.textContent = txt;
  }
  function hidePanel() {
    if (panel) panel.hidden = true;
  }

  function beginCall() {
    var cx = currentContext();
    if (!cx) return;
    showPanel("Llamada");
    setStatus("Resolviendo contacto…");
    apiGet("/api/chatwoot/resolve?account_id=" + cx.accountId + "&conversation_id=" + cx.conversationId)
      .then(function (r) {
        setStatus("Llamando a " + (r.name || r.phone) + "…");
        showPanel(r.name || r.phone);
        setStatus("Llamando…");
        return apiPost("/api/sessions/" + r.session_id + "/calls", { phone: r.phone }).then(function (c) {
          var callId = c.call.callId;
          // WebRTC solo arma el camino de audio navegador↔servidor (ICE). NO
          // arranca el timer: el audio del cliente recién fluye cuando CONTESTA.
          activeCall = startWebRTCCall(
            r.session_id,
            callId,
            function (state, msg) {
              if (state === "error") setStatus("Error: " + (msg || ""));
            },
            function () { activeCall = null; stopDurationTimer(); closeCallEvents(); }
          );
          // El estado REAL de la llamada (suena / contestó / colgó) viene por SSE,
          // reflejando la señalización de WhatsApp.
          openCallEvents(callId);
        });
      })
      .catch(function (err) {
        setStatus("No se pudo llamar: " + err.message);
        setTimeout(hidePanel, 4000);
      });
  }

  function endCall() {
    if (activeCall) activeCall.hangup();
    activeCall = null;
    stopDurationTimer();
    closeCallEvents();
    hidePanel();
  }

  // ---------- estado de la llamada por SSE ----------
  var callES;
  function openCallEvents(callId) {
    closeCallEvents();
    try {
      var url = BASE + "/api/events" + (KEY ? "?apiKey=" + encodeURIComponent(KEY) : "");
      callES = new EventSource(url);
      callES.onmessage = function (e) {
        var m;
        try { m = JSON.parse(e.data); } catch (_) { return; }
        if (!m || m.id !== callId) return;
        if (m.type === "call-status") {
          if (m.status === "connected") {
            if (!durTimer) startDurationTimer(); // el cliente CONTESTÓ
          } else if (m.status === "ringing") {
            if (!durTimer) setStatus("Sonando…");
          } else if (m.status === "ended") {
            endCall();
          }
        } else if (m.type === "call-ended") {
          endCall();
        }
      };
      callES.onerror = function () { /* reintenta solo; el cierre lo maneja endCall */ };
    } catch (_) {}
  }
  function closeCallEvents() {
    if (callES) { try { callES.close(); } catch (_) {} callES = null; }
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

  function ensureButton() {
    // Solo en páginas de conversación.
    if (!currentContext()) {
      var old = document.getElementById(BTN_ID);
      if (old) old.remove();
      return;
    }
    if (document.getElementById(BTN_ID)) return;

    var btn = document.createElement("button");
    btn.id = BTN_ID;
    btn.title = "Llamar por WhatsApp";
    btn.innerHTML = PHONE_SVG;
    btn.style.cssText =
      "position:fixed;right:20px;bottom:80px;z-index:99998;width:48px;height:48px;border:0;border-radius:50%;" +
      "background:#25D366;color:#fff;box-shadow:0 4px 12px rgba(0,0,0,.2);cursor:pointer;display:flex;" +
      "align-items:center;justify-content:center;";
    btn.onclick = beginCall;
    document.body.appendChild(btn);
  }

  var obs = new MutationObserver(function () { ensureButton(); });
  obs.observe(document.body, { childList: true, subtree: true });
  ensureButton();
})();
