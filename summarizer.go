// summarizer.go — Local TextRank topic summarizer (no external LLMs).

package main

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SummarizerConfig controls the TextRank parameters.
type SummarizerConfig struct {
	// TopK is the number of topic sentences to extract.
	TopK int

	// DampingFactor is the PageRank damping factor (typically 0.85).
	DampingFactor float64

	// MaxIterations is the PageRank convergence iteration limit.
	MaxIterations int

	// ConvergenceThreshold: stop when total rank delta < this value.
	ConvergenceThreshold float64

	// MinSentenceLen: ignore sentences shorter than this many characters.
	MinSentenceLen int

	// MaxSentenceLen: truncate sentences longer than this for output.
	MaxSentenceLen int

	// StopWords: language-specific tokens to exclude from TF-IDF.
	StopWords map[string]bool
}

func DefaultSummarizerConfig() SummarizerConfig {
	return SummarizerConfig{
		TopK:                 5,
		DampingFactor:        0.85,
		MaxIterations:        100,
		ConvergenceThreshold: 1e-4,
		MinSentenceLen:       8,
		MaxSentenceLen:       120,
		StopWords:            defaultStopWords(),
	}
}

// TextRankSummarizer summarizes message bodies using the TextRank algorithm.
type TextRankSummarizer struct {
	cfg      SummarizerConfig
	idfCache IDFCache // avoid recomputing IDF on every call
}

func NewTextRankSummarizer(cfg SummarizerConfig) *TextRankSummarizer {
	return &TextRankSummarizer{cfg: cfg}
}

func (s *TextRankSummarizer) Summarize(threadID string, messages []*Message) (*TopicSummary, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	// Determine the actual time range covered by these messages.
	periodStart := messages[0].SentAt
	periodEnd := messages[len(messages)-1].SentAt

	// Helper: return a placeholder summary so the purge loop can record coverage
	// even when the message set has no extractable text (stickers, media-only, etc.).
	mkSummary := func(topics []string, by string) *TopicSummary {
		return &TopicSummary{
			ThreadID: threadID, PeriodStart: periodStart, PeriodEnd: periodEnd,
			Topics: topics, MessageCount: len(messages), GeneratedBy: by, CreatedAt: time.Now(),
		}
	}

	var bodies []string
	for _, m := range messages {
		if m.Body != "" && m.ContentType == ContentText {
			bodies = append(bodies, m.Body)
		}
	}
	if len(bodies) == 0 {
		return mkSummary([]string{"[No text messages in period]"}, "placeholder"), nil
	}

	sentences := s.segmentSentences(strings.Join(bodies, " "))
	if len(sentences) == 0 {
		return mkSummary([]string{"[Could not segment sentences]"}, "placeholder"), nil
	}

	var validSentences []string
	for _, sent := range sentences {
		if len([]rune(sent)) >= s.cfg.MinSentenceLen {
			validSentences = append(validSentences, sent)
		}
	}
	sentences = validSentences
	if len(sentences) == 0 {
		return mkSummary([]string{"[All sentences below minimum length]"}, "placeholder"), nil
	}

	vectors := s.buildTFIDF(sentences)

	matrix := s.buildSimilarityMatrix(sentences, vectors)

	var scores []float64
	if isAllZeroMatrix(matrix) {
			scores = make([]float64, len(sentences))
		for i := range scores {
			scores[i] = 1.0 / float64(len(sentences))
		}
	} else {
		scores = s.pageRank(matrix)
	}

	topics := s.selectTopK(sentences, scores)

	topics = s.augmentWithEntities(topics, strings.Join(bodies, " "))

	return mkSummary(topics, "textrank_v1"), nil
}

var sentenceEnd = regexp.MustCompile(`[.!?。！？…]+\s*`)

func (s *TextRankSummarizer) segmentSentences(text string) []string {
	raw := sentenceEnd.Split(text, -1)
	var result []string
	for _, sentence := range raw {
		sentence = strings.TrimSpace(sentence)
		if sentence != "" {
			result = append(result, sentence)
		}
	}
	return result
}

// tokenize splits a sentence into lowercase tokens.
func (s *TextRankSummarizer) tokenize(sentence string) []string {
	var tokens []string
	runes := []rune(sentence)

	for i := 0; i < len(runes); {
		r := runes[i]

			if isCJK(r) {
			if i+1 < len(runes) && isCJK(runes[i+1]) {
				tok := string(runes[i : i+2])
				if !s.cfg.StopWords[tok] {
					tokens = append(tokens, tok)
				}
			}
			i++
			continue
		}

			if unicode.IsLetter(r) || unicode.IsDigit(r) {
			j := i
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) {
				j++
			}
			tok := strings.ToLower(string(runes[i:j]))
			if len(tok) > 1 && !s.cfg.StopWords[tok] {
				tokens = append(tokens, tok)
			}
			i = j
			continue
		}

		i++
	}

	return tokens
}

func termFrequency(tokens []string) map[string]float64 {
	counts := make(map[string]int)
	for _, t := range tokens { counts[t]++ }
	n := float64(len(tokens))
	tf := make(map[string]float64, len(counts))
	if n == 0 { return tf }
	for t, c := range counts { tf[t] = float64(c) / n }
	return tf
}

func (s *TextRankSummarizer) buildTFIDF(sentences []string) []map[string]float64 {
	tokenLists := make([][]string, len(sentences))
	for i, sent := range sentences {
		tokenLists[i] = s.tokenize(sent)
	}

	idf := s.idfCacheFor(tokenLists, &s.idfCache)

	// Build TF-IDF vectors.
	vectors := make([]map[string]float64, len(sentences))
	for i, toks := range tokenLists {
		tf := termFrequency(toks)
		vec := make(map[string]float64, len(tf))
		for t, tfVal := range tf {
			vec[t] = tfVal * idf[t]
		}
		vectors[i] = vec
	}

	return vectors
}

func cosineSimilarity(a, b map[string]float64) float64 {
	var dot, normA, normB float64
	for t, va := range a { dot += va * b[t]; normA += va * va }
	for _, vb := range b { normB += vb * vb }
	if normA == 0 || normB == 0 { return 0 }
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func isAllZeroMatrix(m [][]float64) bool {
	for i := range m { for j := range m[i] { if m[i][j] > 0 { return false } } }
	return true
}

func (s *TextRankSummarizer) buildSimilarityMatrix(sentences []string, vectors []map[string]float64) [][]float64 {
	n := len(sentences)
	matrix := make([][]float64, n)
	for i := range matrix { matrix[i] = make([]float64, n) }
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			sim := cosineSimilarity(vectors[i], vectors[j])
			matrix[i][j], matrix[j][i] = sim, sim
		}
	}
	for i := 0; i < n; i++ {
		total := 0.0
		for j := 0; j < n; j++ { total += matrix[i][j] }
		if total > 0 {
			for j := 0; j < n; j++ { matrix[i][j] /= total }
		}
	}
	return matrix
}

func (s *TextRankSummarizer) pageRank(matrix [][]float64) []float64 {
	n := len(matrix)
	if n == 0 { return nil }
	scores := make([]float64, n)
	for i := range scores { scores[i] = 1.0 / float64(n) }
	d := s.cfg.DampingFactor
	for iter := 0; iter < s.cfg.MaxIterations; iter++ {
		next := make([]float64, n)
		delta := 0.0
		for i := 0; i < n; i++ {
			rank := (1.0 - d) / float64(n)
			for j := 0; j < n; j++ { rank += d * matrix[j][i] * scores[j] }
			next[i] = rank
			delta += math.Abs(rank - scores[i])
		}
		scores = next
		if delta < s.cfg.ConvergenceThreshold { break }
	}
	return scores
}

type sentenceScore struct {
	index    int
	sentence string
	score    float64
}

func (s *TextRankSummarizer) selectTopK(sentences []string, scores []float64) []string {
	pairs := make([]sentenceScore, len(sentences))
	for i, sent := range sentences {
		pairs[i] = sentenceScore{index: i, sentence: sent, score: scores[i]}
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	k := s.cfg.TopK
	if k > len(pairs) {
		k = len(pairs)
	}

	selected := pairs[:k]
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].index < selected[j].index
	})

	topics := make([]string, 0, k)
	for _, p := range selected {
		topic := p.sentence
		if len([]rune(topic)) > s.cfg.MaxSentenceLen {
			runes := []rune(topic)
			topic = string(runes[:s.cfg.MaxSentenceLen]) + "…"
		}
		topics = append(topics, topic)
	}

	return topics
}

var (
	// datePattern matches common date formats.
	datePattern = regexp.MustCompile(
		`(?i)\b(?:january|february|march|april|may|june|july|august|september|october|november|december)\s+\d{1,2}(?:st|nd|rd|th)?\b|` +
			`\b\d{1,2}[/-]\d{1,2}[/-]\d{2,4}\b|` +
			`\b\d{4}年\d{1,2}月\d{1,2}日\b`, // CJK date format
	)

	// personPattern matches "called|met|saw|talked to + Name".
	personPattern = regexp.MustCompile(
		`(?i)(?:called|emailed|messaged|met|saw|with|from|to)\s+([A-Z][a-z]+(?:\s+[A-Z][a-z]+)?)`,
	)

	// locationPattern matches "in/at + Place".
	locationPattern = regexp.MustCompile(
		`(?i)(?:at|in|near|from)\s+([A-Z][a-zA-Z\s]{3,20})(?:\b)`,
	)

	// actionPattern matches key action phrases.
	actionPattern = regexp.MustCompile(
		`(?i)(?:deadline|meeting|appointment|travel|flight|hotel|payment|order|contract|agreement|project|task)\b[^.!?]{0,60}`,
	)
)

func (s *TextRankSummarizer) augmentWithEntities(existing []string, corpus string) []string {
	var extras []string

	actionMatches := actionPattern.FindAllString(corpus, 3)
	for _, m := range actionMatches {
		m = strings.TrimSpace(m)
		if len(m) > 10 {
			extras = append(extras, m)
		}
	}

	dateMatches := datePattern.FindAllString(corpus, 5)
	if len(dateMatches) > 0 {
		extras = append(extras, "Dates mentioned: "+strings.Join(dateMatches[:min(3, len(dateMatches))], ", "))
	}

	personMatches := personPattern.FindAllStringSubmatch(corpus, 5)
	var persons []string
	seen := make(map[string]bool)
	for _, m := range personMatches {
		if len(m) > 1 && !seen[m[1]] {
			persons = append(persons, m[1])
			seen[m[1]] = true
		}
	}
	if len(persons) > 0 {
		extras = append(extras, "People: "+strings.Join(persons[:min(4, len(persons))], ", "))
	}

	for _, extra := range extras {
		duplicate := false
		for _, e := range existing {
			if strings.Contains(e, extra[:min(20, len(extra))]) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, extra)
		}
	}

	maxTopics := s.cfg.TopK * 2
	if len(existing) > maxTopics {
		existing = existing[:maxTopics]
	}

	return existing
}

func defaultStopWords() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields("a an the and or but in on at to for of with by from up about into through during is are was were be been being have has had do does did will would could should may might shall can it its this that these those i me my we our you your he she his her they their not no so if as just also well get got ok okay yes yeah hi hello hey thanks thank 的 了 在 是 我 有 和 就 不 人 都 一 一个 上 也 很 到 说 要 去 你 会 着 没有 看 好 自己 这 那 来 他 她 们") {
		m[w] = true
	}
	return m
}

// IDFCache stores a pre-computed IDF table with lazy 20%-growth refresh.
type IDFCache struct {
	mu         sync.RWMutex
	idf        map[string]float64
	corpusSize int // number of sentences the IDF was built from
}

// get returns a copy of the cached IDF map and the corpus size it was built for.
func (c *IDFCache) get() (map[string]float64, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.idf == nil {
		return nil, 0
	}
	// Return a read-only view — callers must not mutate it.
	return c.idf, c.corpusSize
}

func (c *IDFCache) set(idf map[string]float64, corpusSize int) {
	c.mu.Lock()
	c.idf = idf
	c.corpusSize = corpusSize
	c.mu.Unlock()
}

// idfCacheFor builds IDF from tokenLists, using the cache when the corpus
func (s *TextRankSummarizer) idfCacheFor(tokenLists [][]string, cache *IDFCache) map[string]float64 {
	n := len(tokenLists)
	if cache != nil {
		if cached, cachedN := cache.get(); cached != nil {
			growth := float64(n-cachedN) / float64(cachedN+1)
			if growth < 0.20 { // corpus grew <20% — reuse cached IDF
				return cached
			}
		}
	}

	df := make(map[string]int, 512)
	for _, toks := range tokenLists {
		seen := make(map[string]bool, len(toks))
		for _, t := range toks {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}
	N := float64(n)
	idf := make(map[string]float64, len(df))
	for t, d := range df {
		idf[t] = math.Log(N / float64(d+1))
	}

	if cache != nil {
		cache.set(idf, n)
	}
	return idf
}
