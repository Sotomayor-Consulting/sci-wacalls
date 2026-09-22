package main

import (
	"context"
	"testing"
	"time"
)

func ownerPtr(s string) *string { return &s }

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
