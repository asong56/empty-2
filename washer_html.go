// washer_html.go — AST-Based HTML Washer (golang.org/x/net/html)
package main

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var trackerDomains = []string{
	"track.", "pixel.", "beacon.", "analytics.", "metrics.",
	"mailtrack.", "mailchimp.", "sendgrid.", "sparkpost.",
	"opens.em.", ".tracking.", "link.hubspot.", "click.em.",
}

func isTrackerURL(rawURL string) bool {
		host := strings.ToLower(rawURL)
	if idx := strings.Index(host, "//"); idx >= 0 {
		host = host[idx+2:]
	}
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	for _, pattern := range trackerDomains {
		if strings.Contains(host, pattern) {
			return true
		}
	}
	return false
}

type htmlWalker struct {
	sb           strings.Builder
	log          []string
	preDepth     int
	listDepth    int
	skipDepth    int
	trackerCount int
	listTypes    []atom.Atom
	listCounters []int
}

func (w *htmlWalker) walkNode(n *html.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.TextNode:
		if w.skipDepth > 0 {
			return
		}
		text := n.Data
		if w.preDepth > 0 {
			w.sb.WriteString(text)
		} else {
			text = strings.ReplaceAll(text, "\n", " ")
			text = strings.ReplaceAll(text, "\t", " ")
			for strings.Contains(text, "  ") {
				text = strings.ReplaceAll(text, "  ", " ")
			}
			w.sb.WriteString(text)
		}
	case html.ElementNode:
		w.walkElement(n)
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walkNode(c)
		}
	}
}

func (w *htmlWalker) walkElement(n *html.Node) {
	tag := n.DataAtom

	switch tag {
	case atom.Script, atom.Style, atom.Head, atom.Noscript, atom.Template, atom.Iframe:
		w.skipDepth++
		w.walkChildren(n)
		w.skipDepth--
		w.logf("blocked <%s>", n.Data)
		return
	}

	if tag == atom.Img {
		src := attrVal(n, "src")
		width, height := attrVal(n, "width"), attrVal(n, "height")
		if (width == "1" || width == "0") && (height == "1" || height == "0") {
			w.trackerCount++
			w.logf("blocked 1×1 pixel img")
			return
		}
		if isTrackerURL(src) {
			w.trackerCount++
			w.logf("blocked tracker img (src=%s)", src)
			return
		}
		alt := attrVal(n, "alt")
		if alt == "" {
			alt = "image"
		}
		w.sb.WriteString(fmt.Sprintf("[Image: %s]", alt))
		return
	}

	if w.skipDepth > 0 {
		w.walkChildren(n)
		return
	}

	switch tag {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(tag-atom.H1) + 1
		w.ensureNewline()
		w.sb.WriteString(strings.Repeat("#", level) + " ")
		w.walkChildren(n)
		w.sb.WriteString("\n\n")
		return
	case atom.P, atom.Div:
		w.ensureNewline()
		w.walkChildren(n)
		w.ensureNewline()
		return
	case atom.Br:
		w.sb.WriteString("\n")
		return
	case atom.Hr:
		w.ensureNewline()
		w.sb.WriteString("---\n")
		return
	case atom.Blockquote:
		w.ensureNewline()
		inner := htmlWalker{}
		inner.walkChildren(n)
		for _, line := range strings.Split(inner.sb.String(), "\n") {
			w.sb.WriteString("> " + line + "\n")
		}
		return
	case atom.Pre:
		w.ensureNewline()
		w.sb.WriteString("```\n")
		w.preDepth++
		w.walkChildren(n)
		w.preDepth--
		w.sb.WriteString("\n```\n")
		return
	case atom.Ul, atom.Ol:
		w.listTypes = append(w.listTypes, tag)
		w.listCounters = append(w.listCounters, 0)
		w.listDepth++
		w.ensureNewline()
		w.walkChildren(n)
		w.listTypes = w.listTypes[:len(w.listTypes)-1]
		w.listCounters = w.listCounters[:len(w.listCounters)-1]
		w.listDepth--
		w.sb.WriteString("\n")
		return
	case atom.Li:
		w.ensureNewline()
		indent := strings.Repeat("  ", w.listDepth-1)
		if len(w.listTypes) > 0 && w.listTypes[len(w.listTypes)-1] == atom.Ol {
			w.listCounters[len(w.listCounters)-1]++
			w.sb.WriteString(fmt.Sprintf("%s%d. ", indent, w.listCounters[len(w.listCounters)-1]))
		} else {
			w.sb.WriteString(indent + "• ")
		}
		w.walkChildren(n)
		w.sb.WriteString("\n")
		return
	case atom.Table:
		w.ensureNewline()
		w.walkChildren(n)
		w.sb.WriteString("\n")
		return
	case atom.Tr:
		w.walkChildren(n)
		w.sb.WriteString("\n")
		return
	case atom.Td, atom.Th:
		w.walkChildren(n)
		w.sb.WriteString(" | ")
		return
	case atom.B, atom.Strong:
		w.wrapChildren(n, "**"); return
	case atom.I, atom.Em:
		w.wrapChildren(n, "_"); return
	case atom.S, atom.Del, atom.Strike:
		w.wrapChildren(n, "~~"); return
	case atom.Code:
		if w.preDepth > 0 { w.walkChildren(n); return }
		w.wrapChildren(n, "`"); return
	case atom.A:
		href := cleanURL(attrVal(n, "href"))
		if isTrackerURL(href) {
			w.walkChildren(n)
			w.logf("stripped tracker link")
			return
		}
		w.sb.WriteString("[")
		w.walkChildren(n)
		w.sb.WriteString(fmt.Sprintf("](%s)", href))
		return
	}
	w.walkChildren(n)
}

func (w *htmlWalker) walkChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walkNode(c)
	}
}

func (w *htmlWalker) wrapChildren(n *html.Node, delim string) {
	w.sb.WriteString(delim)
	w.walkChildren(n)
	w.sb.WriteString(delim)
}

func (w *htmlWalker) ensureNewline() {
	s := w.sb.String()
	if len(s) > 0 && s[len(s)-1] != '\n' {
		w.sb.WriteByte('\n')
	}
}

func (w *htmlWalker) logf(format string, args ...interface{}) {
	w.log = append(w.log, fmt.Sprintf("HTML/AST: "+format, args...))
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func StripHTMLToMarkdown(rawHTML string) (string, []string) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return html.UnescapeString(rawHTML), []string{"HTML parse failed, used fallback"}
	}
	walker := &htmlWalker{}
	walker.walkNode(doc)
	result := normalizeWhitespace(walker.sb.String())
	log := walker.log
	if walker.trackerCount > 0 {
		log = append(log, fmt.Sprintf("blocked %d tracker elements", walker.trackerCount))
	}
	return result, log
}

func ParseAndExtractEmail(rawEmail io.Reader) (string, []string) {
	var log []string
	msg, err := mail.ReadMessage(rawEmail)
	if err != nil {
		log = append(log, fmt.Sprintf("net/mail parse error: %v", err))
		return "", log
	}
	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		body, _ := io.ReadAll(msg.Body)
		log = append(log, "no Content-Type, treating as plain text")
		return strings.TrimSpace(string(body)), log
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		body, _ := io.ReadAll(msg.Body)
		log = append(log, fmt.Sprintf("Content-Type parse error (%v), raw body returned", err))
		return strings.TrimSpace(string(body)), log
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			body, _ := io.ReadAll(msg.Body)
			log = append(log, "multipart boundary missing")
			return strings.TrimSpace(string(body)), log
		}
		text, partLog := extractMultipart(msg.Body, boundary, mediaType)
		return text, append(log, partLog...)
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		log = append(log, fmt.Sprintf("body read error: %v", err))
		return "", log
	}
	if strings.Contains(mediaType, "html") {
		text, htmlLog := StripHTMLToMarkdown(string(body))
		return text, append(log, htmlLog...)
	}
	return strings.TrimSpace(string(body)), log
}

func extractMultipart(body io.Reader, boundary, parentType string) (string, []string) {
	var log []string
	mr := multipart.NewReader(body, boundary)
	type candidate struct {
		mediaType string
		data      []byte
	}
	var candidates []candidate

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log = append(log, fmt.Sprintf("NextPart error: %v", err))
			break
		}
		partCT := part.Header.Get("Content-Type")
		if partCT == "" {
			partCT = "text/plain"
		}
		partMedia, partParams, parseErr := mime.ParseMediaType(partCT)
		if parseErr != nil {
			continue
		}
		if strings.HasPrefix(partMedia, "multipart/") {
			if nested := partParams["boundary"]; nested != "" {
				text, nestedLog := extractMultipart(part, nested, partMedia)
				log = append(log, nestedLog...)
				if text != "" {
					return text, log
				}
			}
			continue
		}
		data, readErr := io.ReadAll(part)
		if readErr != nil {
			continue
		}
		candidates = append(candidates, candidate{partMedia, data})
	}

		if strings.Contains(parentType, "alternative") {
		for _, c := range candidates {
			if c.mediaType == "text/plain" {
				return strings.TrimSpace(string(c.data)), log
			}
		}
		for _, c := range candidates {
			if strings.Contains(c.mediaType, "html") {
				text, htmlLog := StripHTMLToMarkdown(string(c.data))
				return text, append(log, htmlLog...)
			}
		}
	}
	for _, c := range candidates {
		switch {
		case c.mediaType == "text/plain":
			return strings.TrimSpace(string(c.data)), log
		case strings.Contains(c.mediaType, "html"):
			text, htmlLog := StripHTMLToMarkdown(string(c.data))
			return text, append(log, htmlLog...)
		}
	}
	return "", log
}
