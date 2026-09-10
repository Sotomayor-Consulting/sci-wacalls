package main

import "testing"

func TestSelfSentTracking(t *testing.T) {
	s := &Session{}
	if s.isSelfSent("A") {
		t.Fatal("id no marcado no debería ser self-sent")
	}
	s.markSelfSent("A")
	if !s.isSelfSent("A") {
		t.Fatal("id marcado debería ser self-sent")
	}
	if s.isSelfSent("B") {
		t.Fatal("otro id no debería ser self-sent")
	}
	s.markSelfSent("") // id vacío: no-op, no debe romper
	if s.isSelfSent("") {
		t.Fatal("id vacío no debería quedar registrado")
	}
}
