package main

import (
	"log/slog"
	"net"
	"sync/atomic"

	"wacalls/internal/voip/media"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// mediaAPI es la API de pion configurada para NAT (IP pública anunciada + puerto
// UDP fijo). nil = comportamiento por defecto de LAN (candidatos efímeros sobre
// la IP local), que no sirve cuando el navegador está fuera de la red del
// contenedor. La configura setupWebRTCMedia al arrancar.
var mediaAPI *webrtc.API

// setupWebRTCMedia hace que pion anuncie publicIP (NAT 1:1) y multiplexe todo el
// audio WebRTC sobre un único puerto UDP fijo (publicable en Docker/firewall).
// Sin esto, el servidor anuncia la IP interna del contenedor, inalcanzable para
// el navegador → la llamada se queda "conectando" y no fluye audio.
func setupWebRTCMedia(publicIP string, udpPort int, log *slog.Logger) error {
	if udpPort == 0 {
		udpPort = 50000
	}
	// Solo UDP4: por defecto pion intenta también UDP6, enumerando TODAS las
	// direcciones de TODAS las interfaces — incluida la IPv6 link-local que
	// Docker asigna solo en eth0. En algunos hosts (visto en Coolify) el bind a
	// esa dirección falla con "cannot assign requested address" y tumba la
	// función ENTERA (un solo error corta el loop), aunque el bind en IPv4 -que
	// es lo único que se usa, WACALLS_PUBLIC_IP siempre es IPv4- habría andado
	// bien. main.go trata este error como fatal (os.Exit), así que sin este
	// filtro el proceso ni siquiera llega a levantar el servidor HTTP.
	mux, err := ice.NewMultiUDPMuxFromPort(udpPort, ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4))
	if err != nil {
		return err
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)

	ip := publicIP
	if ip == "auto" {
		ip = detectOutboundIP()
	}
	if ip != "" {
		se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
	}
	mediaAPI = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	log.Info("webrtc media configurada", "udp_port", udpPort, "public_ip", ip)
	return nil
}

func newPeerConnection() (*webrtc.PeerConnection, error) {
	if mediaAPI != nil {
		return mediaAPI.NewPeerConnection(webrtc.Configuration{})
	}
	return webrtc.NewPeerConnection(webrtc.Configuration{})
}

// detectOutboundIP devuelve la IP local usada para salir a internet (para
// WACALLS_PUBLIC_IP=auto). En Docker sin host-networking suele ser la IP del
// contenedor, así que en producción conviene fijar la IP pública explícita.
func detectOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// pcmChannelLabel is the data channel the browser opens to carry raw 16 kHz mono
// Int16 LE PCM in both directions. The browser side must create it with this label.
const pcmChannelLabel = "pcm"

// Bridge is the browser-leg adapter: it carries raw PCM between the browser and
// the CallManager over a WebRTC data channel. The call core only ever sees
// []float32 PCM, so it stays unaware of the transport (no Opus here anymore).
type Bridge struct {
	pc  *webrtc.PeerConnection
	dc  atomic.Pointer[webrtc.DataChannel]
	log *slog.Logger

	// OnBrowserPCM is invoked with decoded 16 kHz mono PCM captured from the browser mic.
	OnBrowserPCM func(pcm []float32)
	// OnTerminalICE fires when the peer connection fails or closes.
	OnTerminalICE func()
}

func NewBridge(offerSDP string, log *slog.Logger) (*Bridge, string, error) {
	pc, err := newPeerConnection()
	if err != nil {
		return nil, "", err
	}
	br := &Bridge{pc: pc, log: log}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != pcmChannelLabel {
			return
		}
		br.dc.Store(dc)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if cb := br.OnBrowserPCM; cb != nil && len(msg.Data) > 0 {
				cb(media.PCMInt16LEToFloat32(msg.Data))
			}
		})
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Debug("browser ice state", "state", s.String())
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if br.OnTerminalICE != nil {
				br.OnTerminalICE()
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, "", err
	}
	<-gatherComplete

	return br, pc.LocalDescription().SDP, nil
}

// WritePCM sends 16 kHz mono float32 PCM to the browser as Int16 LE over the data
// channel. It is a no-op until the channel is open.
func (b *Bridge) WritePCM(pcm []float32) error {
	dc := b.dc.Load()
	if dc == nil || len(pcm) == 0 {
		return nil
	}
	return dc.Send(media.PCMFloat32ToInt16LE(pcm))
}

func (b *Bridge) Close() {
	if b.pc != nil {
		_ = b.pc.Close()
	}
}
