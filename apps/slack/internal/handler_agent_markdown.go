package internal

import "strings"

const maxAgentMarkdownLinkBytes = 4096

// hardenAgentMarkdown keeps the agent's standard-Markdown answer renderable while
// removing masked-link ambiguity: [label](url) becomes label (url), so Slack can
// still autolink the destination but the visible text no longer hides it.
func hardenAgentMarkdown(markdown string) string {
	var h agentMarkdownLinkHarden
	return h.write(markdown) + h.flush()
}

type agentMarkdownLinkHarden struct {
	references markdownReferenceDefinitionEscaper

	inCode       bool
	codeTicks    int
	pendingTicks int
	escaped      bool

	pendingBang bool
	link        markdownLinkPending
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

type markdownLinkState int

const (
	markdownLinkNone markdownLinkState = iota
	markdownLinkLabel
	markdownLinkAfterLabel
	markdownLinkDestination
)

func (h *agentMarkdownLinkHarden) write(markdown string) string {
	return h.writeLinks(h.references.write(markdown))
}

func (h *agentMarkdownLinkHarden) writeLinks(markdown string) string {
	var out strings.Builder
	for i := 0; i < len(markdown); i++ {
		c := markdown[i]

	reprocess:
		if h.link.state != markdownLinkNone {
			if !h.consumeLinkByte(&out, c) {
				goto reprocess
			}
			continue
		}
		if c != '`' && h.pendingTicks > 0 {
			h.emitBacktickRun(&out)
		}
		if h.escaped {
			out.WriteByte(c)
			h.escaped = false
			continue
		}
		if h.pendingBang {
			h.pendingBang = false
			if c == '[' && !h.inCode {
				h.startLink(true)
				continue
			}
			out.WriteByte('!')
		}
		if c == '`' {
			h.pendingTicks++
			continue
		}
		if !h.inCode && c == '\\' {
			out.WriteByte(c)
			h.escaped = true
			continue
		}
		if !h.inCode {
			switch c {
			case '!':
				h.pendingBang = true
				continue
			case '[':
				h.startLink(false)
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

func (h *agentMarkdownLinkHarden) flush() string {
	var out strings.Builder
	if ref := h.references.flush(); ref != "" {
		out.WriteString(h.writeLinks(ref))
	}
	if h.pendingTicks > 0 {
		h.emitBacktickRun(&out)
	}
	h.escaped = false
	h.inCode = false
	h.codeTicks = 0
	if h.pendingBang {
		out.WriteByte('!')
		h.pendingBang = false
	}
	if h.link.state != markdownLinkNone {
		out.WriteString(h.link.original.String())
		h.link = markdownLinkPending{}
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
		out.WriteString(h.link.original.String())
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
			out.WriteString(h.link.original.String())
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
	label := strings.TrimSpace(h.link.label.String())
	destination := visibleMarkdownLinkDestination(h.link.destination.String())
	switch {
	case label != "" && destination != "":
		out.WriteString(label)
		out.WriteString(" (")
		out.WriteString(destination)
		out.WriteByte(')')
	case destination != "":
		out.WriteString(destination)
	default:
		out.WriteString(h.link.original.String())
	}
	h.link = markdownLinkPending{}
}

func (h *agentMarkdownLinkHarden) emitBacktickRun(out *strings.Builder) {
	n := h.pendingTicks
	h.pendingTicks = 0
	if n == 0 {
		return
	}
	if h.inCode {
		if n == h.codeTicks {
			h.inCode = false
			h.codeTicks = 0
		}
	} else {
		h.inCode = true
		h.codeTicks = n
	}
	out.WriteString(strings.Repeat("`", n))
}

type markdownReferenceDefinitionEscaper struct {
	lineColumn     int
	lineHasContent bool
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
	for i := 0; i < len(markdown); i++ {
		c := markdown[i]

	reprocess:
		if e.pending.state != markdownReferenceDefinitionNone {
			if !e.consumeReferenceDefinitionByte(&out, c) {
				goto reprocess
			}
			continue
		}
		if c == '[' && !e.lineHasContent && e.lineColumn <= 3 {
			e.startReferenceDefinition()
			continue
		}
		out.WriteByte(c)
		e.advanceLine(c)
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
		out.WriteString(e.pending.original.String())
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
			out.WriteString(e.pending.original.String())
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
	if c == '\n' {
		e.lineColumn = 0
		e.lineHasContent = false
		return
	}
	if c != ' ' || e.lineColumn >= 3 {
		e.lineHasContent = true
	}
	e.lineColumn++
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
