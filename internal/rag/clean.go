package rag

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minBoilerplateDocLines = 30
	minBoilerplateRepeats  = 3
	maxBoilerplateRunes    = 80
)

var (
	pageNumberPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^第\s*\d+\s*页(?:\s*[/,，]?\s*共\s*\d+\s*页)?$`),
		regexp.MustCompile(`(?i)^page\s+\d+(?:\s+(?:of|/)\s+\d+)?$`),
		regexp.MustCompile(`^[-—–]\s*\d+\s*[-—–]$`),
	}
	orderedListPattern = regexp.MustCompile(`^\d+[.)]\s`)
)

// cleanMarkdown removes common conversion noise before chunking while keeping
// fenced code blocks untouched.
func cleanMarkdown(markdown string) string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")
	markdown = stripInvisible(markdown)
	markdown = strings.ReplaceAll(markdown, "\f", "\n")
	markdown = stripHTMLComments(markdown)
	markdown = dropRepeatedBoilerplate(markdown)
	return normalizeCleanWhitespace(markdown)
}

func stripInvisible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\ufeff', '\u200b', '\u200c', '\u200d', '\u2060':
			continue
		}
		if (r < 0x20 && r != '\n' && r != '\t' && r != '\f') || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func stripHTMLComments(markdown string) string {
	lines := strings.Split(markdown, "\n")
	var b strings.Builder
	b.Grow(len(markdown))
	inFence := false
	inComment := false
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			inFence = !inFence
			inComment = false
			b.WriteString(line)
			continue
		}
		if inFence {
			b.WriteString(line)
			continue
		}
		cleaned, open := stripLineComments(line, inComment)
		inComment = open
		b.WriteString(cleaned)
	}
	return b.String()
}

func stripLineComments(line string, inComment bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(line); {
		if inComment {
			j := strings.Index(line[i:], "-->")
			if j < 0 {
				return b.String(), true
			}
			i += j + len("-->")
			inComment = false
			continue
		}
		j := strings.Index(line[i:], "<!--")
		if j < 0 {
			b.WriteString(line[i:])
			break
		}
		b.WriteString(line[i : i+j])
		i += j + len("<!--")
		inComment = true
	}
	return b.String(), inComment
}

// dropRepeatedBoilerplate keeps the first occurrence of short lines that repeat
// across a long document, which removes common headers, footers, and watermarks.
func dropRepeatedBoilerplate(markdown string) string {
	lines := strings.Split(markdown, "\n")
	inFence := false
	nonEmpty := 0
	counts := make(map[string]int)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		nonEmpty++
		if isBoilerplateCandidate(trimmed) {
			counts[trimmed]++
		}
	}
	if nonEmpty < minBoilerplateDocLines {
		return markdown
	}

	repeated := make(map[string]bool, len(counts))
	for line, count := range counts {
		if count >= minBoilerplateRepeats {
			repeated[line] = true
		}
	}
	if len(repeated) == 0 {
		return markdown
	}

	var b strings.Builder
	b.Grow(len(markdown))
	inFence = false
	kept := make(map[string]bool, len(repeated))
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			inFence = !inFence
			b.WriteString(line)
			continue
		}
		if inFence || !repeated[trimmed] {
			b.WriteString(line)
			continue
		}
		if !kept[trimmed] {
			kept[trimmed] = true
			b.WriteString(line)
		}
	}
	return b.String()
}

func isBoilerplateCandidate(line string) bool {
	runes := utf8.RuneCountInString(line)
	if runes < 2 || runes > maxBoilerplateRunes {
		return false
	}
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") ||
		strings.HasPrefix(line, ">") || strings.HasPrefix(line, "- ") ||
		strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") ||
		isFence(line) || orderedListPattern.MatchString(line) {
		return false
	}
	if strings.HasSuffix(line, "。") || strings.HasSuffix(line, "！") ||
		strings.HasSuffix(line, "？") || strings.HasSuffix(line, ".") ||
		strings.HasSuffix(line, "!") || strings.HasSuffix(line, "?") {
		return false
	}
	for _, r := range line {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func normalizeCleanWhitespace(markdown string) string {
	lines := strings.Split(markdown, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	pendingBlank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			if pendingBlank && len(out) > 0 {
				out = append(out, "")
			}
			pendingBlank = false
			inFence = !inFence
			out = append(out, trimmed)
			continue
		}
		if inFence {
			if pendingBlank && len(out) > 0 {
				out = append(out, "")
			}
			pendingBlank = false
			out = append(out, line)
			continue
		}
		if trimmed == "" || isPageNumber(trimmed) {
			if len(out) > 0 {
				pendingBlank = true
			}
			continue
		}
		if pendingBlank {
			out = append(out, "")
			pendingBlank = false
		}
		out = append(out, trimmed)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func isPageNumber(line string) bool {
	for _, pattern := range pageNumberPatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

func dedupeBlocks(blocks []string) []string {
	seen := make(map[string]struct{}, len(blocks))
	out := blocks[:0]
	for _, block := range blocks {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}
		if isHeading(trimmed) {
			out = append(out, block)
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, block)
	}
	return out
}
