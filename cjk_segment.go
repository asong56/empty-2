// cjk_segment.go — Bigram segmenter for CJK-aware FTS5 indexing.

package main

import (
	"strings"
	"unicode"
)

// isCJK reports whether r is a CJK/Kana/Hangul character needing bigram segmentation.
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) || (r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2E80 && r <= 0x2EFF) || (r >= 0x31C0 && r <= 0x31EF) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

func needsNgramSegmentation(r rune) bool {
	return isCJK(r) ||
		(r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) || // Hiragana/Katakana
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul
}

// SegmentCJK emits overlapping CJK bigrams and lowercased Latin tokens.
func SegmentCJK(text string) string {
	if text == "" {
		return ""
	}

	runes := []rune(text)
	var tokens []string

	i := 0
	for i < len(runes) {
		r := runes[i]

		if needsNgramSegmentation(r) {
			start := i
			for i < len(runes) && needsNgramSegmentation(runes[i]) {
				i++
			}
			run := runes[start:i]

			if len(run) == 1 {
					tokens = append(tokens, string(run))
			} else {
				for j := 0; j < len(run)-1; j++ {
					tokens = append(tokens, string(run[j:j+2]))
				}
			}
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i])) {
				i++
			}
			word := strings.ToLower(string(runes[start:i]))
			if len(word) > 0 {
				tokens = append(tokens, word)
			}
			i++
		}
	}

	return strings.Join(tokens, " ")
}

// SegmentCJKQuery segments a search query for FTS5 MATCH (terms AND'd).
func SegmentCJKQuery(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}

	terms := strings.Fields(query)
	var segmented []string
	for _, term := range terms {
		seg := SegmentCJK(term)
		if seg != "" {
			segmented = append(segmented, seg)
		}
	}

	if len(segmented) == 0 {
		return ""
	}
	return strings.Join(segmented, " AND ")
}

func SegmentCJKForIndex(text string) string {
	return SegmentCJK(text) // Same algorithm; alias for clarity at call sites.
}
