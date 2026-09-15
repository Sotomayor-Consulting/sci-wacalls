package main

// Conversión del formato de Chatwoot al de WhatsApp.
//
// El editor de Chatwoot produce Markdown, pero WhatsApp usa su propia sintaxis y
// NO entiende Markdown: un `**negrita**` llega al cliente como los asteriscos
// literales. Las diferencias importantes:
//
//	Markdown            WhatsApp        Resultado
//	**x** / __x__        *x*            negrita
//	*x* / _x_            _x_            itálica   <- el * de Markdown es itálica,
//	                                              pero en WhatsApp es NEGRITA
//	~~x~~                ~x~            tachado
//	`x`                  ```x```        monoespaciado
//
// El caso del asterisco es el que obliga a convertir con cuidado: traducir
// `**x**` a `*x*` y después aplicar la regla de la itálica volvería a tocar lo
// ya convertido. Por eso la negrita se marca con un centinela y se restituye al
// final.

import (
	"regexp"
	"strings"
)

const (
	boldSentinel = "\x01" // marca temporal de negrita ya resuelta
	codeSentinel = "\x02" // marca temporal de bloque de código intacto
)

var (
	// Bloques y tramos de código: se preservan tal cual, sin tocar lo de adentro.
	reCodeFence  = regexp.MustCompile("(?s)```.*?```")
	reCodeInline = regexp.MustCompile("`[^`\n]+`")

	reBoldStars  = regexp.MustCompile(`\*\*([^\n*]+?)\*\*`)
	reBoldUnders = regexp.MustCompile(`__([^\n_]+?)__`)
	reStrike     = regexp.MustCompile(`~~([^\n~]+?)~~`)
	reItalicStar = regexp.MustCompile(`\*([^\n*]+?)\*`)

	// [etiqueta](url) — WhatsApp no tiene enlaces con texto: detecta las URL
	// desnudas, así que se deja la etiqueta seguida de la URL.
	reLink = regexp.MustCompile(`\[([^\]\n]*)\]\((\S+?)\)`)

	// Delimitador de firma de Chatwoot (SIGNATURE_DELIMITER = "--"), que adjunta
	// como "--\n\nFirma". Es convención de correo y en WhatsApp se lee como un
	// renglón de basura.
	reSignatureDelim = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*$\n?`)
	reBlankLines     = regexp.MustCompile(`\n{3,}`)
)

// chatwootToWhatsApp traduce el Markdown de Chatwoot al formato de WhatsApp.
func chatwootToWhatsApp(text string) string {
	if text == "" {
		return text
	}

	// Aparta el código para que su contenido no se reinterprete.
	var code []string
	stash := func(m string) string {
		code = append(code, m)
		return codeSentinel
	}
	out := reCodeFence.ReplaceAllStringFunc(text, stash)
	out = reCodeInline.ReplaceAllStringFunc(out, stash)

	out = reStrike.ReplaceAllString(out, "~${1}~")
	out = reBoldStars.ReplaceAllString(out, boldSentinel+"${1}"+boldSentinel)
	out = reBoldUnders.ReplaceAllString(out, boldSentinel+"${1}"+boldSentinel)
	// Lo que quede con un solo asterisco era itálica en Markdown.
	// OJO con "$1_": en Go el _ es carácter de palabra, así que se
	// interpretaría como el grupo llamado "1_" y expandiría a vacío.
	out = reItalicStar.ReplaceAllString(out, "_${1}_")
	out = strings.ReplaceAll(out, boldSentinel, "*")

	out = reLink.ReplaceAllStringFunc(out, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		label, link := strings.TrimSpace(g[1]), g[2]
		if label == "" || label == link {
			return link
		}
		return label + ": " + link
	})

	// Chatwoot manda "--\n\nFirma": al quitar el delimitador queda un salto de
	// línea de sobra, que en WhatsApp se ve como un hueco.
	out = reSignatureDelim.ReplaceAllString(out, "")
	out = reBlankLines.ReplaceAllString(out, "\n\n")

	// Restituye el código: el inline pasa a monoespaciado de WhatsApp (```),
	// que no tiene equivalente de una sola comilla.
	for _, c := range code {
		repl := c
		if !strings.HasPrefix(c, "```") {
			repl = "```" + strings.Trim(c, "`") + "```"
		}
		out = strings.Replace(out, codeSentinel, repl, 1)
	}
	return strings.TrimRight(out, "\n")
}
