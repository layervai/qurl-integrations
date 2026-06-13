package internal

import "strings"

const (
	maxAgentMarkdownLinkBytes        = 4096
	maxAgentMarkdownNestingDepth     = 32
	maxPartialAngleAutolinkBytes     = 7
	maxVisibleEmailAutolinkLookahead = 320
)

// hardenAgentMarkdown keeps the agent's standard-Markdown answer renderable while
// removing masked-link ambiguity: [label](url) becomes label (url), so Slack can
// still autolink the destination but the visible text no longer hides it. This
// is an anti-phishing visible-destination pass, not a spec-complete Markdown
// parser. Raw HTML tag starts are escaped because they could otherwise become
// another hidden-destination renderer if Slack's markdown surface accepts HTML;
// visible autolinks such as <https://example.com> stay untouched. This stays
// local instead of using a full CommonMark library because streaming deltas must
// be hardened before Slack sees them, even when syntax is split across chunk
// boundaries. The pinned masked-link surface is inline/image links, reference
// definitions, Slack <url|label> angle links, and raw HTML tag starts; keep new
// renderer syntax support pinned by tests.
func hardenAgentMarkdown(markdown string) string {
	return hardenAgentMarkdownWithOptions(markdown, false, 0)
}

// HardenAgentMarkdown exposes the Slack agent reply hardener to cmd-layer
// fallback builders that need the same masked-link neutralization contract.
func HardenAgentMarkdown(markdown string) string {
	return hardenAgentMarkdown(markdown)
}

func hardenAgentMarkdownForStreamReconcile(markdown string) string {
	h := newAgentMarkdownLinkHarden(false, 0)
	h.references.anywhere = true
	return h.write(markdown) + h.flush()
}

func hardenAgentMarkdownWithOptions(markdown string, codeDisabled bool, nestingDepth int) string {
	if nestingDepth > maxAgentMarkdownNestingDepth {
		return escapeMarkdownControlText(markdown)
	}
	h := newAgentMarkdownLinkHarden(codeDisabled, nestingDepth)
	return h.write(markdown) + h.flush()
}

func newAgentMarkdownLinkHarden(codeDisabled bool, nestingDepth int) agentMarkdownLinkHarden {
	return agentMarkdownLinkHarden{codeDisabled: codeDisabled, nestingDepth: nestingDepth}
}

type agentMarkdownLinkHarden struct {
	references markdownReferenceDefinitionEscaper

	inCode       bool
	codeTicks    int
	pendingTicks int
	escaped      bool
	codeDisabled bool
	nestingDepth int
	codeBuffer   strings.Builder

	pendingBang bool
	pendingLess string
	link        markdownLinkPending
	angle       markdownAngleLinkPending
}

type markdownLinkPending struct {
	state        markdownLinkState
	escaped      bool
	labelDepth   int
	destDepth    int
	destSawSpace bool
	destQuote    byte
	original     strings.Builder
	label        strings.Builder
	destination  strings.Builder
}

type markdownAngleLinkPending struct {
	active   bool
	escaped  bool
	sawPipe  bool
	original strings.Builder
	url      strings.Builder
	label    strings.Builder
}

type markdownLinkState int

const (
	markdownLinkNone markdownLinkState = iota
	markdownLinkLabel
	markdownLinkAfterLabel
	markdownLinkDestination
)

func (h *agentMarkdownLinkHarden) write(markdown string) string {
	markdown = h.references.write(markdown)
	if h.pendingLess != "" {
		markdown = h.pendingLess + markdown
		h.pendingLess = ""
	}
	return h.writeLinks(markdown)
}

func (h *agentMarkdownLinkHarden) writeLinks(markdown string) string {
	var out strings.Builder
	for i := 0; i < len(markdown); i++ {
		c := markdown[i]

	reprocess:
		if h.angle.active {
			if !h.consumeAngleLinkByte(&out, c) {
				goto reprocess
			}
			continue
		}
		if h.link.state != markdownLinkNone {
			if !h.consumeLinkByte(&out, c) {
				goto reprocess
			}
			continue
		}
		if !h.consumeMarkdownByte(&out, c, markdown[i:]) {
			break
		}
	}
	return out.String()
}

func (h *agentMarkdownLinkHarden) consumeMarkdownByte(out *strings.Builder, c byte, remaining string) bool {
	if h.pendingTicks > 0 {
		h.emitBacktickRun(out)
	}
	if h.escaped {
		out.WriteByte(c)
		h.escaped = false
		return true
	}
	if h.pendingBang {
		h.pendingBang = false
		if c == '[' {
			h.startLink(true)
			return true
		}
		out.WriteByte('!')
	}
	if !h.codeDisabled && c == '`' {
		h.pendingTicks++
		return true
	}
	if h.inCode {
		h.codeBuffer.WriteByte(c)
		return true
	}
	if c == '\\' {
		out.WriteByte(c)
		h.escaped = true
		return true
	}
	if c == '<' && shouldDeferAngleAutolinkStart(remaining) {
		h.pendingLess = remaining
		return false
	}
	if c == '<' && isSlackControlAngleStart(remaining) {
		out.WriteByte('\\')
		out.WriteByte(c)
		return true
	}
	if c == '<' && isRawHTMLTagStart(remaining) {
		out.WriteByte('\\')
		out.WriteByte(c)
		return true
	}
	if c == '<' && isVisibleAngleLinkStart(remaining) {
		h.startAngleLink()
		return true
	}
	switch c {
	case '!':
		h.pendingBang = true
	case '[':
		h.startLink(false)
	default:
		out.WriteByte(c)
	}
	return true
}

func (h *agentMarkdownLinkHarden) flush() string {
	var out strings.Builder
	if ref := h.references.flush(); ref != "" {
		out.WriteString(h.writeLinks(ref))
	}
	if h.pendingTicks > 0 {
		h.emitBacktickRun(&out)
	}
	if h.inCode {
		out.WriteString(h.flushUnclosedCode())
	}
	h.escaped = false
	h.inCode = false
	h.codeTicks = 0
	if h.pendingBang {
		out.WriteByte('!')
		h.pendingBang = false
	}
	if h.pendingLess != "" {
		out.WriteString(h.pendingLess)
		h.pendingLess = ""
	}
	if h.link.state != markdownLinkNone {
		out.WriteString(h.safeMarkdownLinkOriginal(h.link.original.String()))
		h.link = markdownLinkPending{}
	}
	if h.angle.active {
		out.WriteString(h.escapeMarkdownAngleOriginal())
		h.angle = markdownAngleLinkPending{}
	}
	return out.String()
}

func (h *agentMarkdownLinkHarden) startLink(image bool) {
	h.link = markdownLinkPending{state: markdownLinkLabel}
	if image {
		h.link.original.WriteString("![")
		return
	}
	h.link.original.WriteByte('[')
}

// consumeLinkByte returns false when c was not consumed and must be reprocessed
// as a normal byte after flushing the pending non-link text.
func (h *agentMarkdownLinkHarden) consumeLinkByte(out *strings.Builder, c byte) bool {
	if h.link.original.Len() > maxAgentMarkdownLinkBytes {
		out.WriteString(h.escapeMarkdownLinkOriginal(h.link.original.String()))
		h.link = markdownLinkPending{}
		return false
	}
	switch h.link.state {
	case markdownLinkNone:
		return false
	case markdownLinkLabel:
		h.link.original.WriteByte(c)
		if h.link.escaped {
			h.link.label.WriteByte(c)
			h.link.escaped = false
			return true
		}
		switch c {
		case '\\':
			h.link.label.WriteByte(c)
			h.link.escaped = true
		case '[':
			h.link.labelDepth++
			h.link.label.WriteByte(c)
		case ']':
			if h.link.labelDepth > 0 {
				h.link.labelDepth--
				h.link.label.WriteByte(c)
				return true
			}
			h.link.state = markdownLinkAfterLabel
		default:
			h.link.label.WriteByte(c)
		}
		return true
	case markdownLinkAfterLabel:
		if c != '(' {
			out.WriteString(h.safeMarkdownLinkOriginal(h.link.original.String()))
			h.link = markdownLinkPending{}
			return false
		}
		h.link.original.WriteByte(c)
		h.link.state = markdownLinkDestination
		h.link.destDepth = 1
		h.link.escaped = false
		return true
	case markdownLinkDestination:
		return h.consumeLinkDestinationByte(out, c)
	default:
		return false
	}
}

func (h *agentMarkdownLinkHarden) consumeLinkDestinationByte(out *strings.Builder, c byte) bool {
	h.link.original.WriteByte(c)
	if h.link.escaped {
		h.link.destination.WriteByte(c)
		h.link.escaped = false
		return true
	}
	if h.link.destQuote != 0 {
		h.link.destination.WriteByte(c)
		switch c {
		case '\\':
			h.link.escaped = true
		case h.link.destQuote:
			h.link.destQuote = 0
		}
		return true
	}
	switch c {
	case '\\':
		h.link.destination.WriteByte(c)
		h.link.escaped = true
	case '"', '\'':
		if h.link.destSawSpace {
			h.link.destQuote = c
		}
		h.link.destination.WriteByte(c)
	case ' ', '\t', '\n':
		h.link.destSawSpace = true
		h.link.destination.WriteByte(c)
	case '(':
		h.link.destDepth++
		h.link.destination.WriteByte(c)
	case ')':
		h.link.destDepth--
		if h.link.destDepth == 0 {
			h.emitNeutralizedLink(out)
			return true
		}
		h.link.destination.WriteByte(c)
	default:
		h.link.destination.WriteByte(c)
	}
	return true
}

func (h *agentMarkdownLinkHarden) emitNeutralizedLink(out *strings.Builder) {
	label := strings.TrimSpace(h.hardenNestedMarkdown(h.link.label.String()))
	destination := hardenVisibleMarkdownDestination(visibleMarkdownLinkDestination(h.link.destination.String()))
	switch {
	case label != "" && destination != "":
		out.WriteString(label)
		out.WriteString(" (")
		out.WriteString(destination)
		out.WriteByte(')')
	case destination != "":
		out.WriteString(destination)
	default:
		out.WriteString(h.safeMarkdownLinkOriginal(h.link.original.String()))
	}
	h.link = markdownLinkPending{}
}

func (h *agentMarkdownLinkHarden) startAngleLink() {
	h.angle = markdownAngleLinkPending{active: true}
	h.angle.original.WriteByte('<')
}

func (h *agentMarkdownLinkHarden) consumeAngleLinkByte(out *strings.Builder, c byte) bool {
	if h.angle.original.Len() > maxAgentMarkdownLinkBytes {
		out.WriteString(h.escapeMarkdownAngleOriginal())
		h.angle = markdownAngleLinkPending{}
		return false
	}
	h.angle.original.WriteByte(c)
	if h.angle.escaped {
		h.writeAnglePart(c)
		h.angle.escaped = false
		return true
	}
	switch c {
	case '\\':
		h.writeAnglePart(c)
		h.angle.escaped = true
	case '|':
		if h.angle.sawPipe {
			h.angle.label.WriteByte(c)
			return true
		}
		h.angle.sawPipe = true
	case '>':
		h.emitAngleLink(out)
	default:
		h.writeAnglePart(c)
	}
	return true
}

func (h *agentMarkdownLinkHarden) writeAnglePart(c byte) {
	if h.angle.sawPipe {
		h.angle.label.WriteByte(c)
		return
	}
	h.angle.url.WriteByte(c)
}

func (h *agentMarkdownLinkHarden) emitAngleLink(out *strings.Builder) {
	if !h.angle.sawPipe {
		out.WriteString(h.safeMarkdownAngleOriginal())
		h.angle = markdownAngleLinkPending{}
		return
	}
	label := strings.TrimSpace(h.hardenNestedMarkdown(h.angle.label.String()))
	url := strings.TrimSpace(hardenVisibleMarkdownDestination(h.angle.url.String()))
	switch {
	case label != "" && url != "":
		out.WriteString(label)
		out.WriteString(" (")
		out.WriteString(url)
		out.WriteByte(')')
	case url != "":
		out.WriteString(url)
	default:
		out.WriteString(h.angle.original.String())
	}
	h.angle = markdownAngleLinkPending{}
}

func (h *agentMarkdownLinkHarden) emitBacktickRun(out *strings.Builder) {
	n := h.pendingTicks
	h.pendingTicks = 0
	if n == 0 {
		return
	}
	ticks := strings.Repeat("`", n)
	if h.inCode {
		if n == h.codeTicks {
			h.codeBuffer.WriteString(ticks)
			out.WriteString(h.codeBuffer.String())
			h.inCode = false
			h.codeTicks = 0
			h.codeBuffer.Reset()
			return
		}
		h.codeBuffer.WriteString(ticks)
		return
	}
	h.inCode = true
	h.codeTicks = n
	h.codeBuffer.Reset()
	h.codeBuffer.WriteString(ticks)
}

func (h *agentMarkdownLinkHarden) flushUnclosedCode() string {
	code := h.codeBuffer.String()
	h.inCode = false
	h.codeTicks = 0
	h.codeBuffer.Reset()
	return hardenAgentMarkdownWithCodeDisabled(code)
}

func hardenAgentMarkdownWithCodeDisabled(markdown string) string {
	return hardenAgentMarkdownWithOptions(markdown, true, 0)
}

func (h *agentMarkdownLinkHarden) hardenNestedMarkdown(markdown string) string {
	return hardenAgentMarkdownWithOptions(markdown, false, h.nestingDepth+1)
}

func (h *agentMarkdownLinkHarden) escapeMarkdownLinkOriginal(original string) string {
	return escapeMarkdownLinkOriginal(original, h.nestingDepth)
}

func (h *agentMarkdownLinkHarden) safeMarkdownLinkOriginal(original string) string {
	return safeMarkdownLinkOriginal(original, h.nestingDepth)
}

func (h *agentMarkdownLinkHarden) escapeMarkdownAngleOriginal() string {
	if !h.angle.sawPipe {
		return h.safeMarkdownAngleOriginal()
	}
	return "\\<" + hardenVisibleMarkdownDestination(h.angle.url.String()) + "|" + h.hardenNestedMarkdown(h.angle.label.String())
}

func (h *agentMarkdownLinkHarden) safeMarkdownAngleOriginal() string {
	original := h.angle.original.String()
	if !strings.HasPrefix(original, "<") || !markdownVisibleTextNeedsEscaping(h.angle.url.String()) {
		return original
	}
	return hardenVisibleMarkdownDestination(original)
}

type markdownReferenceDefinitionEscaper struct {
	lineColumn     int
	lineHasContent bool
	anywhere       bool
	pending        markdownReferenceDefinitionPending
}

type markdownReferenceDefinitionPending struct {
	state      markdownReferenceDefinitionState
	escaped    bool
	labelDepth int
	original   strings.Builder
}

type markdownReferenceDefinitionState int

const (
	markdownReferenceDefinitionNone markdownReferenceDefinitionState = iota
	markdownReferenceDefinitionLabel
	markdownReferenceDefinitionAfterLabel
)

func (e *markdownReferenceDefinitionEscaper) write(markdown string) string {
	var out strings.Builder
	// Streaming appends may be parsed as separate Markdown chunks, so a new
	// write boundary can behave like a line start even if prior narration did
	// not end with "\n".
	var chunkColumn int
	var chunkHasContent bool
	for i := 0; i < len(markdown); i++ {
		c := markdown[i]

	reprocess:
		if e.pending.state != markdownReferenceDefinitionNone {
			consumed := e.consumeReferenceDefinitionByte(&out, c)
			if !consumed {
				goto reprocess
			}
			advanceMarkdownLineState(&chunkColumn, &chunkHasContent, c)
			continue
		}
		if c == '[' && (e.anywhere || (!e.lineHasContent && e.lineColumn <= 3) || (!chunkHasContent && chunkColumn <= 3)) {
			e.startReferenceDefinition()
			advanceMarkdownLineState(&chunkColumn, &chunkHasContent, c)
			continue
		}
		out.WriteByte(c)
		e.advanceLine(c)
		advanceMarkdownLineState(&chunkColumn, &chunkHasContent, c)
	}
	return out.String()
}

func (e *markdownReferenceDefinitionEscaper) flush() string {
	if e.pending.state == markdownReferenceDefinitionNone {
		return ""
	}
	original := e.pending.original.String()
	e.pending = markdownReferenceDefinitionPending{}
	return original
}

func (e *markdownReferenceDefinitionEscaper) startReferenceDefinition() {
	e.pending = markdownReferenceDefinitionPending{state: markdownReferenceDefinitionLabel}
	e.pending.original.WriteByte('[')
	e.advanceLine('[')
}

func (e *markdownReferenceDefinitionEscaper) consumeReferenceDefinitionByte(out *strings.Builder, c byte) bool {
	if e.pending.original.Len() > maxAgentMarkdownLinkBytes {
		out.WriteString(escapeMarkdownLinkOriginal(e.pending.original.String(), 0))
		e.pending = markdownReferenceDefinitionPending{}
		return false
	}
	e.pending.original.WriteByte(c)
	e.advanceLine(c)
	if e.pending.escaped {
		e.pending.escaped = false
		return true
	}
	switch e.pending.state {
	case markdownReferenceDefinitionNone:
		return false
	case markdownReferenceDefinitionLabel:
		switch c {
		case '\\':
			e.pending.escaped = true
		case '[':
			e.pending.labelDepth++
		case ']':
			if e.pending.labelDepth > 0 {
				e.pending.labelDepth--
				return true
			}
			e.pending.state = markdownReferenceDefinitionAfterLabel
		case '\n':
			out.WriteString(escapeMarkdownLinkOriginal(e.pending.original.String(), 0))
			e.pending = markdownReferenceDefinitionPending{}
		}
		return true
	case markdownReferenceDefinitionAfterLabel:
		if c == ':' {
			out.WriteByte('\\')
		}
		out.WriteString(e.pending.original.String())
		e.pending = markdownReferenceDefinitionPending{}
		return true
	default:
		return false
	}
}

func (e *markdownReferenceDefinitionEscaper) advanceLine(c byte) {
	advanceMarkdownLineState(&e.lineColumn, &e.lineHasContent, c)
}

func advanceMarkdownLineState(column *int, hasContent *bool, c byte) {
	if c == '\n' {
		*column = 0
		*hasContent = false
		return
	}
	if c != ' ' || *column >= 3 {
		*hasContent = true
	}
	*column++
}

func visibleMarkdownLinkDestination(destination string) string {
	destination = strings.TrimSpace(destination)
	if strings.HasPrefix(destination, "<") {
		if end := strings.IndexByte(destination, '>'); end > 0 {
			return strings.TrimSpace(destination[1:end])
		}
	}
	fields := strings.Fields(destination)
	if len(fields) == 0 {
		return destination
	}
	return fields[0]
}

func isRawHTMLTagStart(s string) bool {
	if len(s) < 2 || s[0] != '<' {
		return false
	}
	return isRawHTMLTagStartAfterLess(s[1:])
}

func isVisibleAngleLinkStart(s string) bool {
	return len(s) >= 2 && s[0] == '<' && isVisibleAngleLinkStartAfterLess(s[1:])
}

func isSlackControlAngleStart(s string) bool {
	return len(s) >= 2 && s[0] == '<' && strings.ContainsRune("@#!", rune(s[1]))
}

func shouldDeferAngleAutolinkStart(s string) bool {
	if s == "" || s[0] != '<' {
		return false
	}
	afterLess := s[1:]
	if afterLess == "" {
		return true
	}
	if len(afterLess) > maxPartialAngleAutolinkBytes {
		return false
	}
	for _, scheme := range []string{"http://", "https://", "mailto:", "tel:"} {
		if len(afterLess) < len(scheme) && hasASCIIPrefixFold(scheme, afterLess) {
			return true
		}
	}
	for i := 0; i < len(afterLess); i++ {
		if !isASCIILetter(afterLess[i]) {
			return false
		}
	}
	return true
}

func hardenVisibleMarkdownDestination(destination string) string {
	if !markdownVisibleTextNeedsEscaping(destination) {
		return destination
	}
	return escapeMarkdownControlText(destination)
}

func markdownVisibleTextNeedsEscaping(text string) bool {
	return strings.ContainsAny(text, "[<")
}

func isVisibleAngleLinkStartAfterLess(s string) bool {
	return hasVisibleAutolinkScheme(s)
}

func isRawHTMLTagStartAfterLess(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '!', '?':
		return true
	case '/':
		return len(s) > 1 && isASCIILetter(s[1])
	default:
		return isASCIILetter(s[0]) && !hasVisibleAutolinkScheme(s) && !looksLikeVisibleEmailAutolink(s)
	}
}

func hasVisibleAutolinkScheme(s string) bool {
	return hasASCIIPrefixFold(s, "http://") ||
		hasASCIIPrefixFold(s, "https://") ||
		hasASCIIPrefixFold(s, "mailto:") ||
		hasASCIIPrefixFold(s, "tel:")
}

func looksLikeVisibleEmailAutolink(s string) bool {
	var at, dotAfterAt bool
	limit := len(s)
	if limit > maxVisibleEmailAutolinkLookahead {
		limit = maxVisibleEmailAutolinkLookahead
	}
	for i := 0; i < limit; i++ {
		switch c := s[i]; {
		case c == '>':
			return at && dotAfterAt
		case c == ' ' || c == '\t' || c == '\n':
			return false
		case c == '@':
			at = i > 0
		case c == '.' && at:
			dotAfterAt = true
		}
	}
	return false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func escapeMarkdownControlText(markdown string) string {
	return strings.NewReplacer("[", "\\[", "<", "\\<").Replace(markdown)
}

func escapeMarkdownLinkOriginal(original string, nestingDepth int) string {
	if nestingDepth >= maxAgentMarkdownNestingDepth {
		return escapeMarkdownControlText(original)
	}
	switch {
	case strings.HasPrefix(original, "!["):
		return "!\\[" + hardenAgentMarkdownWithOptions(strings.TrimPrefix(original, "!["), true, nestingDepth+1)
	case strings.HasPrefix(original, "["):
		return "\\[" + hardenAgentMarkdownWithOptions(strings.TrimPrefix(original, "["), true, nestingDepth+1)
	default:
		return original
	}
}

func safeMarkdownLinkOriginal(original string, nestingDepth int) string {
	if nestingDepth >= maxAgentMarkdownNestingDepth {
		return escapeMarkdownControlText(original)
	}
	switch {
	case strings.HasPrefix(original, "!["):
		tail := strings.TrimPrefix(original, "![")
		hardenedTail := hardenAgentMarkdownWithOptions(tail, true, nestingDepth+1)
		if hardenedTail == tail {
			return original
		}
		return "!\\[" + hardenedTail
	case strings.HasPrefix(original, "["):
		tail := strings.TrimPrefix(original, "[")
		hardenedTail := hardenAgentMarkdownWithOptions(tail, true, nestingDepth+1)
		if hardenedTail == tail {
			return original
		}
		return "\\[" + hardenedTail
	default:
		return original
	}
}
