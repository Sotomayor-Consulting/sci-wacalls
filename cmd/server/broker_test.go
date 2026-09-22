package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func ownerPtr(s string) *string { return &s }

// El QR del pareo (y el auth-state con QR) solo debe llegar al suscriptor que
// pidió el pareo, no a cualquier cliente conectado al broker: escanear el QR
// pairea el dispositivo sin pedir credenciales.
func TestEmitSessionQRDirigido(t *testing.T) {
	b := NewBroker()
	subA := b.subscribe("cliente-A")
	subB := b.subscribe("cliente-B")
	defer b.unsubscribe(subA)
	defer b.unsubscribe(subB)

	b.emitSessionQR("cliente-A", "s-1", "QR-PRIVADO")
	b.emitAuthState("cliente-A", "s-1", AuthSnapshot{State: "qr", QR: "QR-PRIVADO"})

	gotA := collectBrokerEvents(t, subA.ch, 2)
	for _, ev := range gotA {
		var m map[string]any
		if err := json.Unmarshal(ev, &m); err != nil {
			t.Fatalf("evento no-JSON del target: %v", err)
		}
		if m["qr"] == nil || m["qr"] != "QR-PRIVADO" {
			t.Fatalf("el cliente target no recibió el QR: %v", m)
		}
	}
	// El otro cliente no recibe nada: espera un ratito y comprueba que el canal
	// siga vacío (sin bloquear).
	select {
	case ev := <-subB.ch:
		t.Fatalf("el otro cliente recibió un evento: %s", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// Cambios de estado SIN QR (open/logged_out) siguen yendo a todos, porque son
// el estado general de la sesión, no un secreto de pareo.
func TestEmitAuthStateSinQRSeDifunde(t *testing.T) {
	b := NewBroker()
	subA := b.subscribe("cliente-A")
	subB := b.subscribe("cliente-B")
	defer b.unsubscribe(subA)
	defer b.unsubscribe(subB)

	b.emitAuthState("cliente-A", "s-1", AuthSnapshot{State: "open", Paired: true})

	// El otro cliente recibe el evento (sin QR) pero no el QR.
	got := collectBrokerEvents(t, subB.ch, 1)[0]
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	if m["qr"] != nil {
		t.Fatalf("el evento difundido no debería traer QR: %v", m)
	}
}

func collectBrokerEvents(t *testing.T, ch <-chan []byte, n int) [][]byte {
	t.Helper()
	out := make([][]byte, 0, n)
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timeout esperando %d eventos, tengo %d", n, len(out))
		}
	}
	return out
}

func TestOwnerActiveCall(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c1", Owner: ownerPtr("op-A"), Status: StatusConnected})
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c2", Owner: ownerPtr("op-B"), Status: StatusRinging})

	if got := b.ownerActiveCall("op-A"); got != "c1" {
		t.Fatalf("op-A should own c1, got %q", got)
	}
	if got := b.ownerActiveCall("op-C"); got != "" {
		t.Fatalf("op-C owns nothing, got %q", got)
	}
	if got := b.ownerActiveCall(""); got != "" {
		t.Fatalf("empty owner must return empty, got %q", got)
	}

	b.endCall("c1", "done")
	if got := b.ownerActiveCall("op-A"); got != "" {
		t.Fatalf("op-A's call ended, expected empty, got %q", got)
	}
}

// Una saliente que nadie contesta tiene que cortarse sola: WhatsApp manda su
// propio timeout (~45 s) pero no siempre llega, y sin esta red la llamada
// queda viva en el registro y el navegador timbrando para siempre.
func TestCutIfStillRinging(t *testing.T) {
	casos := []struct {
		nombre string
		armar  func(*Broker)
		corta  bool
	}{
		{"sigue timbrando: se corta",
			func(b *Broker) { b.upsertCall(CallRecord{CallID: "c1", Status: StatusRinging}) }, true},
		{"contestada: no se toca",
			func(b *Broker) { b.upsertCall(CallRecord{CallID: "c1", Status: StatusConnected}) }, false},
		{"ya terminada: no se toca",
			func(b *Broker) {
				b.upsertCall(CallRecord{CallID: "c1", Status: StatusRinging})
				b.endCall("c1", "user_ended")
			}, false},
		{"no existe: no se toca", func(b *Broker) {}, false},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			b := NewBroker()
			c.armar(b)
			cortada := false
			cutIfStillRinging(context.Background(), b, "c1", time.Millisecond, func() { cortada = true })
			if cortada != c.corta {
				t.Fatalf("cortada=%v, se esperaba %v", cortada, c.corta)
			}
		})
	}
}

// Si el proceso se está apagando, no hay que cortar nada: teardownAllCalls ya
// se encarga, y actuar acá sería competir con él.
func TestCutIfStillRingingRespetaElContexto(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{CallID: "c1", Status: StatusRinging})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cortada := false
	cutIfStillRinging(ctx, b, "c1", time.Hour, func() { cortada = true })
	if cortada {
		t.Fatal("con el contexto cancelado no debe cortar")
	}
}
