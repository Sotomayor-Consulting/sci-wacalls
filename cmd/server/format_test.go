package main

import "testing"

func TestChatwootToWhatsApp(t *testing.T) {
	casos := []struct{ nombre, in, want string }{
		{"negrita markdown -> negrita whatsapp",
			"Hola **Juan**", "Hola *Juan*"},
		{"negrita con guiones bajos",
			"Hola __Juan__", "Hola *Juan*"},
		{"itálica markdown con asterisco -> guion bajo",
			"esto es *importante*", "esto es _importante_"},
		{"itálica ya en formato whatsapp se conserva",
			"esto es _importante_", "esto es _importante_"},
		{"tachado",
			"~~cancelado~~", "~cancelado~"},
		{"negrita e itálica juntas",
			"**Total:** *USD 100*", "*Total:* _USD 100_"},
		{"no confundir negrita con itálica al convertir",
			"**a** y *b*", "*a* y _b_"},
		{"enlace con etiqueta",
			"ver [la factura](https://x.com/f.pdf)", "ver la factura: https://x.com/f.pdf"},
		{"enlace sin etiqueta queda solo la url",
			"[](https://x.com)", "https://x.com"},
		{"firma: se quita el delimitador de correo",
			"Saludos\n\n--\n\nJefferson Rios\nSCI", "Saludos\n\nJefferson Rios\nSCI"},
		{"código inline pasa a monoespaciado de whatsapp",
			"usá `npm install`", "usá ```npm install```"},
		{"no se toca el formato dentro de un bloque de código",
			"```\n**literal**\n```", "```\n**literal**\n```"},
		{"texto sin formato queda igual",
			"Estimado cliente, su orden S00800D está lista.", "Estimado cliente, su orden S00800D está lista."},
		{"asterisco suelto no rompe",
			"2 * 3 = 6", "2 * 3 = 6"},
		{"vacío", "", ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if got := chatwootToWhatsApp(c.in); got != c.want {
				t.Errorf("in=%q\n got=%q\nwant=%q", c.in, got, c.want)
			}
		})
	}
}
