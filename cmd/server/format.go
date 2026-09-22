package main

// Conversión del formato de texto de Chatwoot al de WhatsApp.
//
// El editor de Chatwoot compone en Markdown y WhatsApp no lo entiende: sin
// traducir, un `**negrita**` llega al cliente con los asteriscos literales.
//
// Solo se traducen negrita e itálica, que es lo único que el editor ofrece para
// un inbox de tipo API — su mapa de capacidades declara `marks: ['strong','em']`
// y ningún nodo. El resto del mensaje se deja intacto, firma incluida.
//
//	Markdown        WhatsApp     Resultado
//	**x** / __x__    *x*         negrita
//	*x*              _x_         itálica
//	_x_              _x_         itálica (ya venía bien)
//
// El asterisco es el motivo de que esto no sea un reemplazo directo: en Markdown
// significa itálica y en WhatsApp negrita. Convertir `**x**` a `*x*` y después
// aplicar la regla de la itálica volvería a tocar lo ya convertido, así que la
// negrita se marca con un centinela y se restituye al final.

import (
	"regexp"
	"strings"
)

const boldSentinel = "\x01" // marca temporal de negrita ya resuelta

var (
	reBoldStars  = regexp.MustCompile(`\*\*([^\n*]+?)\*\*`)
	reBoldUnders = regexp.MustCompile(`__([^\n_]+?)__`)
	reItalicStar = regexp.MustCompile(`\*([^\n*]+?)\*`)
)

// chatwootToWhatsApp traduce negritas e itálicas de Markdown al formato de
// WhatsApp, y recorta los espacios de los extremos. Lo demás pasa sin cambios.
func chatwootToWhatsApp(text string) string {
	if text == "" {
		return text
	}
	out := reBoldStars.ReplaceAllString(text, boldSentinel+"${1}"+boldSentinel)
	out = reBoldUnders.ReplaceAllString(out, boldSentinel+"${1}"+boldSentinel)
	// Lo que quede con un solo asterisco era itálica en Markdown.
	// OJO con "$1_": en Go el _ es carácter de palabra, así que se interpretaría
	// como el grupo llamado "1_" y expandiría a vacío.
	out = reItalicStar.ReplaceAllString(out, "_${1}_")
	out = strings.ReplaceAll(out, boldSentinel, "*")
	// El editor de Chatwoot deja saltos de línea al final cuando se envía con
	// Ctrl+Enter (visto: "Prueba de envio\n\n\n"): en ese modo Enter inserta
	// salto en vez de enviar, y esos quedan en el contenido. WhatsApp los
	// respeta y el globo aparece inflado con espacio vacío debajo del texto.
	// Los saltos INTERNOS no se tocan: solo se recortan los extremos.
	return strings.TrimSpace(out)
}
