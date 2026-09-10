package main

// Grabación de llamadas: acumula el PCM de ambos lados (peer/WhatsApp y
// navegador/agente), lo mezcla en una pista mono y lo encoda como WAV
// (PCM 16 bits, 16 kHz) — sin dependencias externas (no ffmpeg). El WAV se sube
// a Chatwoot como nota privada al terminar la llamada (ver finalizeRecording).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
)

const (
	recSampleRate = 16000
	recMinSeconds = 3                             // llamadas muy cortas no generan archivo
	recMaxSamples = recSampleRate * 60 * 60       // tope de seguridad: 1 h por lado
)

// callRecorder acumula el audio de una llamada. Los writes vienen de dos
// goroutines distintas (audio del peer y del navegador), de ahí el mutex.
type callRecorder struct {
	mu      sync.Mutex
	peer    []float32
	browser []float32
	closed  bool
}

func newCallRecorder() *callRecorder { return &callRecorder{} }

// writePeer/writeBrowser son no-op si el recorder es nil (grabación desactivada),
// para poder llamarlos sin chequear en el hot path del audio.
func (r *callRecorder) writePeer(pcm []float32) {
	if r == nil {
		return
	}
	r.appendSide(&r.peer, pcm)
}
func (r *callRecorder) writeBrowser(pcm []float32) {
	if r == nil {
		return
	}
	r.appendSide(&r.browser, pcm)
}

func (r *callRecorder) appendSide(dst *[]float32, pcm []float32) {
	if len(pcm) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(*dst) >= recMaxSamples {
		return
	}
	*dst = append(*dst, pcm...)
}

// finishWAV cierra la grabación, mezcla ambos lados y devuelve el WAV, la
// duración en segundos y ok=false si la llamada fue demasiado corta.
func (r *callRecorder) finishWAV() (data []byte, seconds int, ok bool) {
	r.mu.Lock()
	r.closed = true
	peer, browser := r.peer, r.browser
	r.mu.Unlock()

	n := max(len(peer), len(browser))
	if n < recSampleRate*recMinSeconds {
		return nil, 0, false
	}
	pcm16 := make([]int16, n)
	for i := 0; i < n; i++ {
		var s float32
		if i < len(peer) {
			s += peer[i]
		}
		if i < len(browser) {
			s += browser[i]
		}
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		pcm16[i] = int16(s * 32767)
	}
	return encodeWAV(pcm16, recSampleRate), n / recSampleRate, true
}

// encodeWAV escribe un WAV mono PCM 16 bits little-endian.
func encodeWAV(samples []int16, sampleRate int) []byte {
	const (
		bitsPerSample = 16
		numChannels   = 1
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(samples) * 2

	var b bytes.Buffer
	b.Grow(44 + dataSize)
	// RIFF header
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataSize))
	b.WriteString("WAVE")
	// fmt chunk
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16)) // PCM fmt chunk size
	binary.Write(&b, binary.LittleEndian, uint16(1))  // PCM
	binary.Write(&b, binary.LittleEndian, uint16(numChannels))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(byteRate))
	binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(bitsPerSample))
	// data chunk
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataSize))
	for _, s := range samples {
		binary.Write(&b, binary.LittleEndian, s)
	}
	return b.Bytes()
}

func fmtDuration(seconds int) string {
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}
