// washer.go — Content Washer Pipeline
// Pipeline: Raw Bytes → Blocked? → Extract → Markdown → Strip HTML → Normalize → Truncate
package main

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var bufPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}

var (
	htmlIndicatorRe        = regexp.MustCompile(`(?i)<(?:html|head|body|div|span|p|table|td|tr|img|a\s|br|hr|h[1-6])[^>]*>`)
	normalizeMultiNewlineRe = regexp.MustCompile(`\n{3,}`)
	normalizeHspaceRe      = regexp.MustCompile(`[ \t]+`)
	urlRe                  = regexp.MustCompile(`https?://[^\s<>"]+`)
	brTagRe                = regexp.MustCompile(`(?i)<br\s*/?>|</?p[^>]*>`)
	liTagRe                = regexp.MustCompile(`(?i)<li[^>]*>`)
	boundaryRe             = regexp.MustCompile(`(?i)boundary=["']?([^\s"';:\r\n]+)["']?`)
	mdH1         = regexp.MustCompile(`(?i)<h1[^>]*>(.*?)</h1>`)
	mdH2         = regexp.MustCompile(`(?i)<h2[^>]*>(.*?)</h2>`)
	mdH3         = regexp.MustCompile(`(?i)<h3[^>]*>(.*?)</h3>`)
	mdBold       = regexp.MustCompile(`(?i)<(?:b|strong)[^>]*>(.*?)</(?:b|strong)>`)
	mdItalic     = regexp.MustCompile(`(?i)<(?:i|em)[^>]*>(.*?)</(?:i|em)>`)
	mdStrike     = regexp.MustCompile(`(?i)<(?:del|s|strike)[^>]*>(.*?)</(?:del|s|strike)>`)
	mdCode       = regexp.MustCompile(`(?i)<code[^>]*>(.*?)</code>`)
	mdPre        = regexp.MustCompile(`(?i)<pre[^>]*>(.*?)</pre>`)
	mdLink       = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	mdBlockquote = regexp.MustCompile(`(?i)<blockquote[^>]*>(.*?)</blockquote>`)
	mdHR         = regexp.MustCompile(`(?i)<hr\s*/?>`)
	extractJSONFieldCache sync.Map // string → *regexp.Regexp
)

var trackingParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"fbclid", "gclid", "mc_eid", "yclid", "ref", "_hsenc", "_hsmi",
}

type WasherConfig struct {
	MaxBodyRunes        int
	StripImages         bool
	ConvertToMarkdown   bool
	BlockedContentTypes []string
	MaxImageBytes       int64
	MaxVideoBytes       int64
}

func DefaultWasherConfig() WasherConfig {
	return WasherConfig{
		MaxBodyRunes:        4096,
		StripImages:         true,
		ConvertToMarkdown:   true,
		BlockedContentTypes: []string{"sticker", "red_packet", "mini_program", "location_card", "gif_sticker", "marketing"},
		MaxImageBytes:       10 * 1024 * 1024,
		MaxVideoBytes:       100 * 1024 * 1024,
	}
}

type ContentWasher struct{ cfg WasherConfig }

func NewContentWasher(cfg WasherConfig) *ContentWasher { return &ContentWasher{cfg: cfg} }

type WashResult struct {
	Body        string
	ContentType ContentType
	Redirected  *RedirectedPayload
	Media       *MediaRef
	Log         []string
	BytesIn     int
	BytesOut    int
}

func (w *ContentWasher) Wash(protocol SourceProtocol, upstreamType string, rawContent []byte) WashResult {
	var wsLog []string
	logf := func(format string, args ...interface{}) {
		wsLog = append(wsLog, fmt.Sprintf(format, args...))
	}
	result := WashResult{BytesIn: len(rawContent)}
	if w.isBlocked(upstreamType) {
		logf("blocked type=%q", upstreamType)
		result.ContentType = ContentRedirected
		result.Redirected = w.buildRedirect(protocol, upstreamType)
		result.Log = wsLog
		return result
	}
	var rawText string
	switch protocol {
	case ProtoEmail:
		rawText = w.extractEmail(rawContent, logf)
	case ProtoWeChat, ProtoWhatsApp, ProtoSignal:
		rawText = w.extractIMMessage(rawContent, upstreamType)
	default:
		rawText = string(rawContent)
	}
	if looksLikeMarketing(rawText) {
		logf("marketing content detected")
		result.ContentType = ContentRedirected
		result.Redirected = w.buildRedirect(protocol, "marketing")
		result.Log = wsLog
		return result
	}
	if w.cfg.ConvertToMarkdown && looksLikeHTML(rawText) {
		rawText = w.toMarkdown(rawText)
	}
	if looksLikeHTML(rawText) {
		rawText = w.stripHTML(rawText, logf)
	}
	rawText = normalizeWhitespace(rawText)
	if runeCount := utf8.RuneCountInString(rawText); runeCount > w.cfg.MaxBodyRunes {
		rawText = truncateRunes(rawText, w.cfg.MaxBodyRunes) + "\n\n…[truncated]"
	}
	result.Body = rawText
	result.ContentType = ContentText
	result.BytesOut = len(rawText)
	result.Log = wsLog
	return result
}

func (w *ContentWasher) isBlocked(upstreamType string) bool {
	t := strings.ToLower(upstreamType)
	for _, blocked := range w.cfg.BlockedContentTypes {
		if t == blocked {
			return true
		}
	}
	return false
}

var marketingKeywords = []string{
	"unsubscribe", "click here to", "limited offer", "exclusive deal",
	"buy now", "shop now", "free shipping", "discount code", "promo code",
	"% off", "flash sale", "act now", "don't miss out", "last chance",
	"优惠", "折扣", "促销", "限时", "点击领取", "立即购买",
}

func looksLikeMarketing(body string) bool {
	lower, n := strings.ToLower(body), 0
	for _, kw := range marketingKeywords {
		if strings.Contains(lower, kw) {
			if n++; n >= 2 { return true }
		}
	}
	return false
}

var deepLinkTemplates = map[SourceProtocol]map[string]string{
	ProtoWeChat: {"red_packet": "weixin://dl/redpacket", "mini_program": "weixin://dl/miniapp"},
}

var displayLabels = map[string]string{
	"sticker":       "[Sticker Intercepted]",
	"gif_sticker":   "[Sticker Intercepted]",
	"red_packet":    "[Red Packet — tap to open in WeChat]",
	"mini_program":  "[Mini Program — tap to open in WeChat]",
	"location_card": "[Location — view on Google Maps]",
	"marketing":     "[Marketing message filtered]",
}

func (w *ContentWasher) buildRedirect(protocol SourceProtocol, upstreamType string) *RedirectedPayload {
	label, ok := displayLabels[upstreamType]
	if !ok {
		label = fmt.Sprintf("[%s — open in official app]", titleCase(strings.ReplaceAll(upstreamType, "_", " ")))
	}
	deepLink := ""
	if protoLinks, ok := deepLinkTemplates[protocol]; ok {
		deepLink = protoLinks[upstreamType]
	}
	if upstreamType == "location_card" {
		deepLink = "https://maps.google.com/maps?q="
	}
	return &RedirectedPayload{OriginalType: upstreamType, DisplayLabel: label, DeepLink: deepLink}
}

func (w *ContentWasher) extractEmail(raw []byte, logf func(string, ...interface{})) string {
	text, log := ParseAndExtractEmail(bytes.NewReader(raw))
	for _, entry := range log {
		logf("email: %s", entry)
	}
	if text != "" {
		return text
	}
	logf("email: net/mail returned empty — falling back to regex extractor")
	content := string(raw)
	if plain := extractMIMEPart(content, "text/plain"); plain != "" {
		return plain
	}
	if htmlPart := extractMIMEPart(content, "text/html"); htmlPart != "" {
		return htmlPart
	}
	return content
}

func (w *ContentWasher) extractIMMessage(raw []byte, upstreamType string) string {
	content := string(raw)
	switch upstreamType {
	case "text", "chat":
		if body := extractJSONField(content, "body"); body != "" {
			return body
		}
	case "image", "video", "audio", "file":
		return extractJSONField(content, "caption")
	}
	return content
}

var trackerPatterns = func() []*regexp.Regexp {
	pats := []string{
		`(?i)<img[^>]+src=["'][^"']*(?:track|pixel|beacon|open|click|analytics)[^"']*["'][^>]/?>`,
		`(?i)<img[^>]+(?:width|height)=["']?1["']?[^>]*/?>`,
		`(?i)<(?:iframe|script|style)[^>]*>[\s\S]*?</(?:iframe|script|style)>`,
		`(?i)<link[^>]+rel=["']?stylesheet["']?[^>]*/?>`,
		`(?i)\s+(?:utm_source|utm_medium|utm_campaign|fbclid|gclid|mc_eid)=[^\s&"'>]+`,
	}
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats { out[i] = regexp.MustCompile(p) }
	return out
}()

var (
	htmlTagStrip   = regexp.MustCompile(`<[^>]+>`)
	htmlEntityRegex = regexp.MustCompile(`&(?:#\d+|[a-zA-Z]+);`)
	htmlEntityMap  = map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'",
		"&nbsp;": " ", "&mdash;": "—", "&ndash;": "–", "&hellip;": "…",
		"&laquo;": "«", "&raquo;": "»",
	}
)

func (w *ContentWasher) stripHTML(input string, logf func(string, ...interface{})) string {
	result := input
	for _, pattern := range trackerPatterns {
		before := result
		result = pattern.ReplaceAllString(result, "")
		if result != before {
			logf("tracker removed")
		}
	}
	result = brTagRe.ReplaceAllString(result, "\n")
	result = liTagRe.ReplaceAllString(result, "\n• ")
	result = htmlTagStrip.ReplaceAllString(result, "")
	result = htmlEntityRegex.ReplaceAllStringFunc(result, func(entity string) string {
		if r, ok := htmlEntityMap[entity]; ok {
			return r
		}
		if strings.HasPrefix(entity, "&#") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(entity, "&#"), ";")
			var code int
			fmt.Sscanf(numStr, "%d", &code)
			return string(rune(code))
		}
		return entity
	})
	return stripTrackingParams(result)
}

func stripTrackingParams(text string) string {
	return urlRe.ReplaceAllStringFunc(text, cleanURL)
}

func cleanURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	changed := false
	for _, param := range trackingParams {
		if q.Has(param) {
			q.Del(param)
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (w *ContentWasher) toMarkdown(input string) string {
	r := input
	r = mdH1.ReplaceAllString(r, "# $1")
	r = mdH2.ReplaceAllString(r, "## $1")
	r = mdH3.ReplaceAllString(r, "### $1")
	r = mdBold.ReplaceAllString(r, "**$1**")
	r = mdItalic.ReplaceAllString(r, "_$1_")
	r = mdStrike.ReplaceAllString(r, "~~$1~~")
	r = mdCode.ReplaceAllString(r, "`$1`")
	r = mdPre.ReplaceAllString(r, "```\n$1\n```")
	r = mdLink.ReplaceAllString(r, "[$2]($1)")
	r = mdBlockquote.ReplaceAllString(r, "> $1")
	r = mdHR.ReplaceAllString(r, "---")
	return r
}

func normalizeWhitespace(s string) string {
	s = normalizeMultiNewlineRe.ReplaceAllString(s, "\n\n")
	s = normalizeHspaceRe.ReplaceAllString(s, " ")
	parts := strings.Split(s, "\n")
	for i, p := range parts { parts[i] = strings.TrimRight(p, " \t") }
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func truncateRunes(s string, n int) string {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	count := 0
	for _, r := range s {
		if count >= n {
			break
		}
		buf.WriteRune(r)
		count++
	}
	return buf.String()
}

func looksLikeHTML(s string) bool { return htmlIndicatorRe.MatchString(s) }

func extractMIMEPart(content, mimeType string) string {
	matches := boundaryRe.FindStringSubmatch(content)
	if len(matches) < 2 {
		if strings.Contains(strings.ToLower(content[:min(200, len(content))]), mimeType) {
			if idx := strings.Index(content, "\r\n\r\n"); idx != -1 {
				return content[idx+4:]
			}
			if idx := strings.Index(content, "\n\n"); idx != -1 {
				return content[idx+2:]
			}
		}
		return ""
	}
	boundary := "--" + matches[1]
	for _, part := range strings.Split(content, boundary) {
		if strings.Contains(strings.ToLower(part[:min(200, len(part))]), mimeType) {
			if idx := strings.Index(part, "\r\n\r\n"); idx != -1 {
				return strings.TrimSpace(part[idx+4:])
			}
			if idx := strings.Index(part, "\n\n"); idx != -1 {
				return strings.TrimSpace(part[idx+2:])
			}
		}
	}
	return ""
}

func extractJSONField(jsonStr, field string) string {
	var pattern *regexp.Regexp
	if v, ok := extractJSONFieldCache.Load(field); ok {
		pattern = v.(*regexp.Regexp)
	} else {
		pattern = regexp.MustCompile(fmt.Sprintf(`(?i)"%s"\s*:\s*"((?:[^"\\]|\\.)*)"`, regexp.QuoteMeta(field)))
		extractJSONFieldCache.Store(field, pattern)
	}
	m := pattern.FindStringSubmatch(jsonStr)
	if len(m) < 2 {
		return ""
	}
	return strings.NewReplacer(`\"`,`"`,`\\`,`\`,`\n`,"\n",`\r`,"",`\t`,"\t").Replace(m[1])
}

type MediaProxyJob struct {
	JobID         string
	Protocol      SourceProtocol
	UpstreamURL   string
	UpstreamType  string
	OriginalSize  int64
	TargetFormat  string
	MaxOutputSize int64
	EnqueuedAt    time.Time
	ExpiresAt     time.Time
	PurgeOnView   bool
}

func EnqueueMediaProxy(job MediaProxyJob) MediaRef {
	return MediaRef{
		ServerPath:   fmt.Sprintf("/media/jobs/%s", job.JobID),
		StreamURL:    fmt.Sprintf("/media/stream/%s", job.JobID),
		ThumbURL:     fmt.Sprintf("/media/thumb/%s", job.JobID),
		MIMEType:     mimeForFormat(job.TargetFormat),
		OriginalSize: job.OriginalSize,
		ExpiresAt:    job.ExpiresAt,
		PurgeOnView:  job.PurgeOnView,
	}
}

func mimeForFormat(format string) string {
	if m := map[string]string{"webp":"image/webp","hls":"application/x-mpegURL","opus":"audio/ogg; codecs=opus"}[format]; m != "" {
		return m
	}
	return "application/octet-stream"
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 { words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:]) }
	}
	return strings.Join(words, " ")
}
