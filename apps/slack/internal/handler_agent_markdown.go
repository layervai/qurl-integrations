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
	inCode    bool
	codeTicks int

	pendingBang bool
	link        markdownLinkPending
}

type markdownLinkPending struct {
	state       markdownLinkState
	escaped     bool
	labelDepth  int
	destDepth   int
	original    strings.Builder
	label       strings.Builder
	destination strings.Builder
}

type markdownLinkState int

const (
	markdownLinkNone markdownLinkState = iota
	markdownLinkLabel
	markdownLinkAfterLabel
	markdownLinkDestination
)

func (h *agentMarkdownLinkHarden) write(markdown string) string {
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
		if h.pendingBang {
			h.pendingBang = false
			if c == '[' && !h.inCode {
				h.startLink(true)
				continue
			}
			out.WriteByte('!')
		}
		if c == '`' {
			n := countByteRun(markdown[i:], '`')
			ticks := markdown[i : i+n]
			if h.inCode {
				if n == h.codeTicks {
					h.inCode = false
					h.codeTicks = 0
				}
			} else {
				h.inCode = true
				h.codeTicks = n
			}
			out.WriteString(ticks)
			i += n - 1
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
		h.link.original.WriteByte(c)
		if h.link.escaped {
			h.link.destination.WriteByte(c)
			h.link.escaped = false
			return true
		}
		switch c {
		case '\\':
			h.link.destination.WriteByte(c)
			h.link.escaped = true
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
	default:
		return false
	}
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

func countByteRun(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] != b {
			return i
		}
	}
	return len(s)
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
