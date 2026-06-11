// Package agent implements the qURL Secure Access Agent conversation mode: a
// natural-language translation + preview layer that sits on top of the
// deterministic slash-command operations.
//
// The agent never mutates infrastructure directly. Read-only tools (listing
// resources, listing aliases, resolving a token, quota) execute inline, scoped
// to what the calling user can see in the current channel. Anything that would
// protect, revoke, or grant access is emitted as a [Proposal] — the loop stops
// and hands the proposal back to the caller, which renders a confirm card and
// only mutates on an explicit human click (re-running the admin gate). The LLM's
// fuzziness therefore stays strictly outside the default-deny security boundary;
// it only makes asking easier.
//
// The package depends on two injected ports — [LLM] and [Backend] — so the loop
// is unit-testable without a network or a live Slack workspace.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Conversation roles. Kept as constants so the loop and the SDK translation
// layer agree on the exact wire values.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// defaultMaxIterations bounds the read-tool gathering loop. A natural-language
// request resolves to a handful of read tool calls before the agent either
// answers or proposes a mutation; the cap is a backstop against a model that
// loops without converging, not a normal exit.
const defaultMaxIterations = 6

// iterationCapMessage is returned as the reply when the loop hits
// [defaultMaxIterations] without the model answering or proposing — a graceful
// "ask me again" rather than a silent stall.
const iterationCapMessage = "I wasn't able to work that out — could you rephrase, or use a `/qurl` command directly?"

// proposalAckResult is the synthetic tool_result recorded for a propose_* tool
// call when the loop stops to await confirmation. It keeps the persisted
// conversation history well-formed (every tool_use is followed by a
// tool_result), so a later turn in the same thread is a valid request.
const proposalAckResult = "Proposed to the user for confirmation."

// ActionKind names the mutation an agent [Proposal] represents. Each maps to an
// existing deterministic slash operation that the confirm path executes after a
// human click + admin re-check.
type ActionKind string

// Recognized proposal actions.
const (
	// ActionGet mints a one-time access link for a tunnel slug or channel alias.
	ActionGet ActionKind = "get"
	// ActionRevoke revokes a protected resource (and all its qURLs).
	ActionRevoke ActionKind = "revoke"
	// ActionSetAlias binds a channel alias to a tunnel slug.
	ActionSetAlias ActionKind = "set_alias"
	// ActionUnsetAlias clears a channel alias.
	ActionUnsetAlias ActionKind = "unset_alias"
	// ActionProtectConnector protects a connector (FRP-backed reverse tunnel).
	ActionProtectConnector ActionKind = "protect_connector"
	// ActionProtectURL protects an existing URL.
	ActionProtectURL ActionKind = "protect_url"
)

// ToolCall is a single tool invocation requested by the model.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the result of executing a read tool (or a placeholder recorded
// when the loop stops to propose a mutation), fed back to the model.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one turn of conversation history. It is JSON-serializable so the
// Slack layer can persist a thread's history to DynamoDB between turns. A user
// message carries either Text (a typed message) or ToolResults (read-tool
// outputs); an assistant message carries Text and/or ToolCalls.
type Message struct {
	Role        string       `json:"role"`
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// TurnContext carries the immutable per-turn identity the read tools and the
// system prompt need. The Backend uses it to scope every read to what this
// caller can see in this channel.
type TurnContext struct {
	TeamID        string
	EnterpriseID  string
	ChannelID     string
	ChannelName   string
	UserID        string
	CallerIsAdmin bool
}

// ToolSpec is a tool definition handed to the model: a name, a description, and
// a JSON-Schema object whose properties are Schema with the listed Required
// keys.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
	Required    []string
}

// Request is one round-trip to the model: the system prompt, the available
// tools, and the conversation so far.
//
// The system prompt is split so the constant part can be cached. SystemStable is
// byte-identical across every turn and thread (the role + rules preamble); the
// concrete LLM marks it (and the tool definitions, which render before it) as a
// cache breakpoint so the agent's up-to-6 round-trips per turn don't reprocess it
// each time. SystemPerTurn holds the per-turn context (channel/user/admin), which
// varies and sits after the cached prefix.
type Request struct {
	SystemStable  string
	SystemPerTurn string
	Tools         []ToolSpec
	Messages      []Message
}

// Usage is the model's token accounting for a request, including the cache
// counters used to confirm whether prompt caching is paying off.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// add accumulates another round-trip's usage into u.
func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheCreationInputTokens += o.CacheCreationInputTokens
	u.CacheReadInputTokens += o.CacheReadInputTokens
}

// Response is the model's reply for one round-trip: any assistant text, any
// requested tool calls, the stop reason, and token usage.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	Usage      Usage
}

// LLM is the port to the language model. The concrete implementation
// ([NewAnthropicLLM]) wraps the Anthropic Go SDK; tests inject a fake.
type LLM interface {
	Complete(ctx context.Context, req *Request) (Response, error)
}

// streamingLLM is the optional streaming extension to [LLM]. When the configured
// LLM implements it AND a per-turn stream sink is set ([WithStreamSink]), [Run]
// forwards the assistant's text deltas to the sink as the model generates them,
// instead of waiting for the whole round-trip. It's a separate interface (asserted
// at the seam, like io.WriterTo) so every existing non-streaming [LLM] fake stays
// valid — they simply take the [LLM.Complete] path. onText receives only non-empty
// text deltas; the full [Response] (text, tool calls, usage) is still returned when
// the round-trip ends, identical to [LLM.Complete].
type streamingLLM interface {
	StreamComplete(ctx context.Context, req *Request, onText func(delta string)) (Response, error)
}

// Backend is the port to qURL read operations, each scoped by the
// implementation to what the caller (in [TurnContext]) may see in their channel.
// Every method returns a model-readable summary string (the tool_result the LLM
// sees) plus an error. Implementations live in the Slack layer and are backed by
// the qURL client + channel_policies; the agent package stays free of those
// types so it can be tested with a fake.
type Backend interface {
	// ListResources lists the resources reachable from the caller's channel.
	ListResources(ctx context.Context, tc *TurnContext) (string, error)
	// ListAliases lists the channel-scoped aliases visible to the caller.
	ListAliases(ctx context.Context, tc *TurnContext) (string, error)
	// ResolveToken resolves a $slug/$alias to a resource identity, channel
	// scoped and read-only (it never mints or grants access).
	ResolveToken(ctx context.Context, tc *TurnContext, token string) (string, error)
	// Quota reports the workspace plan and usage.
	Quota(ctx context.Context, tc *TurnContext) (string, error)
}

// Proposal is a mutation the agent wants to perform, awaiting human
// confirmation. The confirm path re-resolves and validates these fields, shows
// the resolved resource identity, re-checks the admin gate when AdminGated, and
// only then executes via the shared mutation core.
type Proposal struct {
	Action     ActionKind
	Token      string // the $slug/$alias to act on, for get and revoke
	Target     string // set-alias target slug
	Alias      string // the alias name for set-alias / unset-alias; suggested alias for protect
	URL        string // protect-url target
	Env        string // protect-connector environment
	Port       string // protect-connector local port
	Reason     string // audit reason distilled from the natural-language intent
	Summary    string // one-sentence description for the confirm card
	AdminGated bool   // whether the confirm path must re-check CheckAdmin
}

// Result is the outcome of a turn: exactly one of Reply (text to post in-thread,
// which may be a clarifying question) or Proposal (a mutation awaiting confirm),
// plus Usage summed across the turn's round-trips (for cost/cache observability).
type Result struct {
	Reply    string
	Proposal *Proposal
	Usage    Usage
}

// Agent runs the conversation loop over an [LLM] and a [Backend].
type Agent struct {
	llm           LLM
	backend       Backend
	maxIterations int
	// streamSink, when non-nil and the LLM implements [streamingLLM], receives the
	// assistant's text deltas as they stream. Per-turn: the handler constructs a
	// fresh Agent for each turn, so the sink never outlives the turn it serves.
	streamSink func(delta string)
}

// Option configures an [Agent].
type Option func(*Agent)

// WithMaxIterations overrides the default read-tool loop cap.
func WithMaxIterations(n int) Option {
	return func(a *Agent) {
		if n > 0 {
			a.maxIterations = n
		}
	}
}

// WithStreamSink wires a per-turn sink that receives the assistant's text deltas
// as they stream. nil (the default) keeps [Run] on the non-streaming
// [LLM.Complete] path; a non-nil sink only takes effect when the configured LLM
// also implements [streamingLLM]. The sink is called from the loop goroutine, in
// order, with each non-empty text delta.
//
// The sink observes EVERY round's text, not just the final reply's. A tool-calling
// round's text (rare — the system prompt leads with the answer, not preamble) and a
// propose_* round's narration both stream before [Run] decides the turn's outcome:
// there's no lookahead to suppress them, and the stream is append-only. So a turn
// that ends in a [Proposal] may still have streamed narration first — [Run] returns
// the [Proposal] regardless (the streamed text is the agent "speaking" before the
// confirm card follows). The caller's stream finalization handles that case.
func WithStreamSink(sink func(delta string)) Option {
	return func(a *Agent) { a.streamSink = sink }
}

// New constructs an Agent. llm and backend are required.
func New(llm LLM, backend Backend, opts ...Option) *Agent {
	a := &Agent{llm: llm, backend: backend, maxIterations: defaultMaxIterations}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// errMissingDeps guards against a zero-value Agent reaching the LLM.
var errMissingDeps = errors.New("agent: llm and backend are required")

// Run executes one user turn. It appends the user's message to history, runs the
// read-tool loop, and returns either a text reply/question or a [Proposal],
// along with the updated history to persist for the next turn. The returned
// history is always well-formed (every tool_use has a matching tool_result),
// even when the loop stops to propose.
func (a *Agent) Run(ctx context.Context, tc *TurnContext, history []Message, userText string) (Result, []Message, error) {
	if a.llm == nil || a.backend == nil {
		return Result{}, history, errMissingDeps
	}

	// Split the system prompt: the stable preamble + tools are the cache prefix;
	// the per-turn context varies and sits after it.
	perTurn := turnContextLines(tc)
	tools := toolSpecs()

	// Copy history so we never mutate the caller's slice; append the new turn.
	msgs := make([]Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, Message{Role: roleUser, Text: userText})

	var usage Usage
	for range a.maxIterations {
		resp, err := a.complete(ctx, &Request{SystemStable: systemPreamble, SystemPerTurn: perTurn, Tools: tools, Messages: msgs})
		if err != nil {
			return Result{Usage: usage}, msgs, fmt.Errorf("agent: llm complete: %w", err)
		}
		usage.add(resp.Usage)

		// Record the assistant turn before acting on it, so the persisted
		// history stays a faithful transcript.
		msgs = append(msgs, Message{Role: roleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			// Guard against the model returning neither text nor a tool call —
			// posting an empty message would be worse than asking again.
			reply := resp.Text
			if strings.TrimSpace(reply) == "" {
				reply = iterationCapMessage
			}
			return Result{Reply: reply, Usage: usage}, msgs, nil
		}

		// Parallel tool use is disabled, so there is normally one call; handle
		// a slice defensively. A propose_* call ends the turn immediately.
		results := make([]ToolResult, 0, len(resp.ToolCalls))
		for i := range resp.ToolCalls {
			call := resp.ToolCalls[i]
			prop, isPropose, perr := parseProposal(call)
			switch {
			case isPropose && perr != nil:
				// Malformed proposal input — feed the error back so the model
				// can correct on the next iteration rather than failing the turn.
				results = append(results, ToolResult{ToolUseID: call.ID, Content: perr.Error(), IsError: true})
			case isPropose:
				// Stop the turn here. Every tool_use in this assistant turn still
				// needs a matching tool_result: the reads already handled (in
				// results), this proposal call, and any calls after it this turn —
				// drained as acks so the transcript stays well-formed even if
				// parallel tool use is ever enabled.
				results = append(results, ToolResult{ToolUseID: call.ID, Content: proposalAckResult})
				for _, rest := range resp.ToolCalls[i+1:] {
					results = append(results, ToolResult{ToolUseID: rest.ID, Content: proposalAckResult})
				}
				msgs = append(msgs, Message{Role: roleUser, ToolResults: results})
				return Result{Proposal: prop, Usage: usage}, msgs, nil
			default:
				content, isErr := a.executeRead(ctx, tc, call)
				results = append(results, ToolResult{ToolUseID: call.ID, Content: content, IsError: isErr})
			}
		}
		msgs = append(msgs, Message{Role: roleUser, ToolResults: results})
	}

	return Result{Reply: iterationCapMessage, Usage: usage}, msgs, nil
}

// complete runs one model round-trip, routing to the streaming path when a per-turn
// sink is set and the LLM implements [streamingLLM], else the plain [LLM.Complete].
// Centralizing the choice here keeps [Run]'s loop identical for both paths.
func (a *Agent) complete(ctx context.Context, req *Request) (Response, error) {
	if a.streamSink != nil {
		if s, ok := a.llm.(streamingLLM); ok {
			return s.StreamComplete(ctx, req, a.streamSink)
		}
	}
	return a.llm.Complete(ctx, req)
}
