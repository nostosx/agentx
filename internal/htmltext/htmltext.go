// Package htmltext extracts readable text from HTML. It is a lightweight
// conversion, not a full HTML parser, shared by the web_fetch tool and the
// rag HTML parser.
package htmltext

import (
	"html"
	"regexp"
	"strings"
)

var (
	reInvisible = regexp.MustCompile(`(?is)<script\b.*?</script\s*>|<style\b.*?</style\s*>|<noscript\b.*?</noscript\s*>|<template\b.*?</template\s*>|<!--.*?-->`)
	reBreak     = regexp.MustCompile(`(?i)<br\s*/?>|</?(p|div|section|article|header|footer|nav|main|aside|li|ul|ol|dl|dt|dd|h[1-6]|tr|table|pre|blockquote|figure|hr)\b[^>]*>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces    = regexp.MustCompile(`[ \t\f\v\r]+`)
	reTitle     = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
)

// Text extracts readable text from HTML. Scripts, styles and comments are
// dropped, block elements start new lines and entities are decoded.
func Text(s string) string {
	s = reInvisible.ReplaceAllString(s, "")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// Block elements each start a line, so blank lines carry no structure.
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(reSpaces.ReplaceAllString(line, " ")); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Title returns the text of the document's <title> element, or "".
func Title(s string) string {
	m := reTitle.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	title := html.UnescapeString(reTag.ReplaceAllString(m[1], ""))
	return strings.TrimSpace(reSpaces.ReplaceAllString(strings.ReplaceAll(title, "\n", " "), " "))
}
