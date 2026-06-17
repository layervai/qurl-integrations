// Package internal contains Slack-specific handler logic.
package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	authFailureMessage       = "Failed to authenticate. Please check your qURL API key configuration."
	workspaceNotSetupMessage = "qURL isn't connected to this workspace yet. Run `/qurl setup <email>` to connect it."
)

// ErrSlackTriggerExpired lets Config.OpenView report Slack's short-lived
// trigger_id expiry distinctly from auth, network, and Slack API failures.
var ErrSlackTriggerExpired = errors.New("slack trigger_id expired")

// ErrSlackRateLimited lets Config.OpenView surface Slack views.open rate
// limiting distinctly so the slash-command follow-up can give the operator a
// retry-shaped action instead of a generic setup failure.
var ErrSlackRateLimited = errors.New("slack views.open rate limited")

// ErrSlackMissingScope lets Slack Web API adapters surface `missing_scope` as
// a reauthorization problem instead of a generic delivery failure.
var ErrSlackMissingScope = errors.New("slack missing_scope")

var errSetupUsage = errors.New("setup usage")

type setupCommand struct {
	email string
	mode  oauth.SetupMode
}

// SlackRateLimitError preserves Slack's Retry-After hint while still matching
// [ErrSlackRateLimited] through errors.Is. OpenView implementations return it
// when Slack includes a concrete retry delay.
type SlackRateLimitError struct {
	RetryAfter string
}

func (e *SlackRateLimitError) Error() string {
	if e == nil || e.RetryAfter == "" {
		return ErrSlackRateLimited.Error()
	}
	return fmt.Sprintf("%s: retry_after=%s", ErrSlackRateLimited, e.RetryAfter)
}

func (e *SlackRateLimitError) Unwrap() error {
	return ErrSlackRateLimited
}

// NewSlackRateLimitError wraps a non-empty Retry-After header; empty headers
// fall back to the sentinel so callers do not need separate branching.
func NewSlackRateLimitError(retryAfter string) error {
	retryAfter = strings.TrimSpace(retryAfter)
	if retryAfter == "" {
		return ErrSlackRateLimited
	}
	return &SlackRateLimitError{RetryAfter: retryAfter}
}

// OpenViewFunc posts a Slack modal through `views.open`.
type OpenViewFunc func(ctx context.Context, teamID, triggerID string, viewJSON []byte) error

// PostFeedbackFunc delivers a `/qurl feedback` submission to the internal
// feedback Slack channel by POSTing a Block Kit payload to a Slack incoming
// webhook. The bytes are the full webhook request body (built by
// [FeedbackMessage]); the implementation owns only the HTTP delivery.
type PostFeedbackFunc func(ctx context.Context, payload []byte) error

// SlackRateLimitRetryAfter returns Slack's Retry-After hint from err when the
// OpenView implementation preserved one with [NewSlackRateLimitError].
func SlackRateLimitRetryAfter(err error) string {
	var rateLimitErr *SlackRateLimitError
	if errors.As(err, &rateLimitErr) && rateLimitErr != nil {
		return rateLimitErr.RetryAfter
	}
	return ""
}

// authErrorMessage maps an APIKey-lookup error to the right user-facing
// reply. The ErrWorkspaceNotConfigured sentinel is the "admin hasn't run
// /qurl setup yet" path — surface a useful next-action instead of the
// generic auth-failure string.
func authErrorMessage(err error) string {
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return workspaceNotSetupMessage
	}
	return authFailureMessage
}

// rateLimitErrorMessage maps the rate-limit gate's defensive storage errors to
// user-facing copy. Most failures are DDB/service-health failures, but the
// workspace-not-bound branch can happen if an admin unbinds the workspace
// between the caller's auth lookup and the limiter's own existence read.
func rateLimitErrorMessage(err error) string {
	var storeErr *slackdata.Error
	if errors.As(err, &storeErr) &&
		storeErr.StatusCode == http.StatusNotFound &&
		storeErr.Code == slackdata.ErrCodeWorkspaceNotBound {
		return workspaceUnboundReply
	}
	return serviceUnreachableMessage
}

// ackWorkingOnIt is the user-visible ephemeral text returned synchronously
// while async work runs. The hourglass keeps the user oriented that a
// follow-up via response_url is on its way.
const ackWorkingOnIt = ":hourglass: Working on it…"

// ackBusy is returned when the bounded async pool is saturated. Surfacing
// this to the user (rather than silently dropping) makes back-pressure
// visible and gives them an actionable next step.
const ackBusy = ":warning: Secure Access Agent is busy — please retry in a moment."

// modalBusyMsg is the modal-surface counterpart to ackBusy, shown when the
// async pool is saturated and a modal action can't be served. A single const
// keeps the wording identical across every modal handler.
const modalBusyMsg = "Secure Access Agent is busy. Retry in a moment."

const (
	headerSlackSignature = "X-Slack-Signature"
	headerSlackTimestamp = "X-Slack-Request-Timestamp"
	setupVerb            = "setup"
	uninstallVerb        = "uninstall"
	setupFlagRotate      = "--rotate"
	setupFlagRepoint     = "--repoint"
)

const (
	pathHealth            = "/health"
	pathSlackCommands     = "/slack/commands"
	pathSlackEvents       = "/slack/events"
	pathSlackInteractions = "/slack/interactions"
	healthStatusKey       = "status"
	healthStatusOK        = "ok"
	healthStatusDraining  = "draining"
)

// Slash-command names. Both POST to pathSlackCommands with the same HMAC
// signature gate; Slack stamps which command the user invoked in the
// `command` form field, and handleSlashCommand dispatches on it. The
// admin command is a deploy prerequisite — it must be registered in the
// Slack app config pointing at the same request URL as commandUser, or
// these literals never arrive (see the package README / PR notes).
const (
	commandUser  = "/qurl"
	commandAdmin = "/qurl-admin"
)

// adminCommandSuffix is how every env names its admin slash command: the
// user command plus this suffix (`/qurl`→`/qurl-admin`,
// `/qurl-sandbox`→`/qurl-sandbox-admin`; see qurl-integrations-infra
// slack-manifests/envs.json). handleSlashCommand classifies on the suffix
// rather than the literal commandAdmin so a non-prod env whose commands
// carry an env infix still reaches the admin surface instead of falling
// through to the user one.
//
// SCOPE OF THE REWRITE: only the help text is rewritten to the invoked
// command name (userHelpMessage / adminHelpMessage, via ReplaceAll). Static
// error / usage / retry copy elsewhere (tunnelInstallUsage, the alias usage
// strings, modal-error and rate-limit messages, empty-state hints) bakes in
// the prod `/qurl-admin` / `/qurl` literal and is deliberately NOT rewritten.
// That drift is accepted: non-prod (env-infix) installs are operator-only
// internal sandboxes, where a retry hint naming the prod command is a
// non-issue, and customer installs are always prod where the literals are
// already correct. Threading the invoked command through every error site
// isn't worth the churn for that audience.
const adminCommandSuffix = "-admin"

// isAdminCommand reports whether the invoked slash command is the admin
// surface — any `/qurl-…-admin` command, not just the prod commandAdmin.
// Scoped to the `/qurl-` command family (so `/qurl-admin`,
// `/qurl-sandbox-admin` match but a stray `/qurlfoo-admin` or
// `/some-other-admin` doesn't) — a foreign command can't route to admin
// dispatch and then have userCommandName/help name a sibling that doesn't
// exist.
//
// Classification is best-effort and assumes Slack only dispatches the
// commands registered for this app (the `/qurl[-env][-admin]` family). An
// unregistered shape like `/qurl-admin-extra` (suffix `-extra`, not `-admin`)
// would fall through to the user surface and render its own name in help —
// harmless, because Slack never sends a command that wasn't registered.
func isAdminCommand(command string) bool {
	return strings.HasPrefix(command, commandUser+"-") && strings.HasSuffix(command, adminCommandSuffix)
}

// userCommandName / adminCommandName return the sibling command names for
// the invoked command, so wrong-surface redirects and help name the
// command that actually exists in this workspace (e.g.
// `/qurl-sandbox-admin`, not the prod `/qurl-admin`). Both are idempotent
// on their own surface: userCommandName(commandUser)==commandUser and
// adminCommandName(commandAdmin)==commandAdmin.
//
// The returned names are Slack-stamped command literals (a registered slash
// command, never free-form user input), so the redirect/unknown-subcommand
// replies interpolate them into backtick fences raw — only the user-typed
// verb text is run through echoText. That keeps the no-op hygiene off trusted
// literals; if Slack's command tokens ever stopped being backtick-free this
// assumption (documented on isAdminCommand) would need revisiting.
func userCommandName(command string) string {
	return strings.TrimSuffix(command, adminCommandSuffix)
}

func adminCommandName(command string) string {
	return userCommandName(command) + adminCommandSuffix
}

const (
	// defaultMaxConcurrentAsync caps in-flight goroutines. A Slack-side
	// flood (replay storm, runaway integration) drops with ackBusy past
	// this threshold rather than unbounded-spawning until the task OOMs.
	// Guided tunnel setup acks before its async admin check to preserve
	// Slack's trigger_id window, so keep this high enough that admin-check
	// retries cannot starve normal async replies. 50 is generous for
	// steady-state — the target customer (50 active users) won't sustain
	// >1 click/sec across the whole workspace.
	defaultMaxConcurrentAsync = 50

	// defaultMaxConcurrentFollowupAsync sizes the SEPARATE pool that admitted channel
	// thread follow-up turns run on, so a busy channel's follow-up work can't saturate
	// the main pool that @mention/DM/slash/interaction work shares (#712). Main-pool
	// isolation holds at ANY size; tune via QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_ASYNC
	// at enablement from real saturation metrics, not a guess.
	defaultMaxConcurrentFollowupAsync = defaultMaxConcurrentAsync

	// defaultMaxConcurrentFollowupGateAsync bounds the short DDB admission check for
	// channel thread follow-ups separately from the long-running follow-up turn pool.
	// This keeps unrelated channel chatter from spending all follow-up turn slots on
	// "is this our thread?" reads, while still capping the read fan-out against the
	// agent-state table during message.channels bursts (#719).
	defaultMaxConcurrentFollowupGateAsync = 10

	// asyncWorkTimeout caps how long a single async job may run. Slack's
	// response_url is valid for 30 minutes, but in practice qURL API calls
	// resolve in <1s; 25s is the deadline beyond which the user is better
	// served by a "failed" follow-up than an indefinite "Working on it…".
	//
	// Interaction with WithRetry(2): the qURL client uses exponential
	// backoff with a 30s cap (shared/client.defaultMaxDelay). Under a
	// 5xx storm, retry backoff alone could in principle exceed the
	// remaining ctx budget — `c.waitForRetry` honors ctx and returns
	// ctx.Err() in that case, which surfaces as a non-*APIError and
	// hits sanitizeAPIError's prefix-only fallback. Trade-off is
	// intentional: cap the user's wait at this deadline rather than
	// let retries dominate.
	asyncWorkTimeout = 25 * time.Second

	// responseURLTimeout bounds the POST to Slack's response_url. Slack's
	// hooks endpoint typically responds in <500ms; 5s catches transient
	// blips without holding a goroutine slot for the full asyncWorkTimeout.
	responseURLTimeout = 5 * time.Second
)

// maxRequestBodyBytes caps the request body the handler will read. Slack
// slash-command and event payloads are well under 8 KiB; 1 MiB gives
// generous headroom while keeping a single bad client from forcing the
// task to allocate unbounded memory.
//
// HMAC verification needs the raw body, so we can't authenticate before
// reading — a flood of cap-sized junk to /slack/* with bad signatures
// will allocate up to 1 MiB per request. ALB-level rate-limiting / WAF
// is the real defense; this cap bounds the per-request damage.
const maxRequestBodyBytes = 1 << 20

// internalErrorEnvelope is the fallback 500 body used when JSON marshal
// of a richer payload fails (unreachable for current callers).
const internalErrorEnvelope = `{"error":"internal"}`

// Config carries the runtime wiring for [NewHandler]. Every field is
// captured by value into [Handler.cfg] once and then read on the
// request hot path without synchronization — callers MUST NOT mutate
// the originating Config after the call. Wiring that has to be
// (re)set after NewHandler returns goes through a SetX setter with an
// explicit double-wire panic (see [Handler.SetAliasStore] /
// [Handler.SetOAuthSetup]); do not add a "swap PostDM / OpenView at
// runtime" path without that same posture.
type Config struct {
	AuthProvider       auth.Provider
	SlackSigningSecret string
	NewClient          func(apiKey string) *client.Client

	// BaseContext is the server-lifetime parent of every async work
	// goroutine's context. SIGTERM cancels it, which propagates to
	// in-flight qURL API calls and response_url POSTs so they release
	// the worker slot promptly during shutdown. Defaults to
	// context.Background() if nil — fine for tests, not for production
	// (cmd/main.go threads the signal-canceled context).
	BaseContext context.Context

	// MaxConcurrentAsync caps in-flight async goroutines. Zero or
	// negative falls back to defaultMaxConcurrentAsync.
	MaxConcurrentAsync int

	// MaxConcurrentFollowupAsync sizes the separate channel-follow-up pool (#712). Zero or
	// negative falls back to defaultMaxConcurrentFollowupAsync.
	MaxConcurrentFollowupAsync int

	// MaxConcurrentFollowupGateAsync sizes the short channel-follow-up admission gate
	// pool (#719). Zero or negative falls back to defaultMaxConcurrentFollowupGateAsync.
	MaxConcurrentFollowupGateAsync int

	// AgentAckTimeout bounds each best-effort Slack "working on it" seam
	// (reactions.add, reactions.remove, and assistant-pane setStatus). Zero or
	// negative falls back to defaultAgentAckTimeout. Tests may shrink it to cover
	// timeout paths without spending the production budget in wall-clock time.
	AgentAckTimeout time.Duration

	// ResponseURLClient is the HTTP client used to POST follow-up
	// messages to Slack's response_url. Nil means "use a default *http.Client
	// with responseURLTimeout"; tests inject one to assert payloads.
	ResponseURLClient *http.Client

	// AdminStore is the DDB-direct facade for workspace_mappings +
	// channel_policies. When nil, the admin verbs short-circuit to a
	// graceful "admin features are not configured" reply — fine for
	// sandbox / no-DDB tests. Production wires one in cmd/main.go
	// from the QURL_*_TABLE env vars (see slackdata.NewStore).
	AdminStore *slackdata.Store

	// OpenView posts a `views.open` Slack web API call to display a
	// modal in response to a slash command. The token owner parameter is
	// usually the workspace team_id; Enterprise Grid org installs can pass
	// enterprise_id instead while the modal metadata remains workspace-scoped.
	// Legacy single-workspace deploys can still fall back to one
	// SLACK_BOT_TOKEN. Tests inject a stub that records the call. Tunnel
	// install uses this for guided setup; setalias-rebind can use the same seam
	// for confirmation modals.
	OpenView OpenViewFunc

	// SlackInstallURL starts the Slack app install/reauthorization flow that
	// stores the per-workspace bot token used by OpenView. When set, guided
	// setup can give sandbox admins a direct recovery link instead of an
	// operator-only reinstall prompt.
	SlackInstallURL string

	// PostDM posts a direct message via chat.postMessage on the
	// per-workspace bot token, with the same Enterprise Grid fallback as
	// OpenView/PostMessage. `/qurl get dm:true` and qURL Connector
	// bootstrap-secret delivery both rely on this privacy seam; nil keeps
	// those secret-bearing flows fail-closed before minting.
	PostDM PostDMFunc

	// TunnelImage is the Docker image shown by `/qurl-admin protect-connector`.
	// The public env var is QURL_CONNECTOR_IMAGE; this field keeps the
	// historical tunnel naming used by the install-rendering code.
	// Empty falls back to the public client image with the `latest` tag only for
	// explicit dev/sandbox installs; production cmd/main.go fails closed unless
	// the operator sets a specific non-latest tag or digest.
	// Tag/digest policy is enforced by cmd/main.go, not by the renderer, so tests
	// that construct Config directly must pass a pinned image unless they
	// intentionally exercise the dev/sandbox fallback path.
	TunnelImage string

	// PostFeedback delivers a `/qurl feedback` submission to the internal
	// feedback Slack channel. Nil disables `/qurl feedback`: the command
	// replies that feedback isn't enabled and userHelpMessage omits the line.
	// Production wires it in cmd/main.go from FEEDBACK_SLACK_WEBHOOK_URL.
	PostFeedback PostFeedbackFunc

	// --- Conversation mode (Secure Access Agent over the Events API) ---
	// The feature is OFF unless AgentLLM, AgentStore, and PostMessage are all
	// wired AND AgentDisabled is false. Leaving any nil keeps it dark — which is
	// how production stays during the staged rollout until the enablement wiring
	// (LLM key, state table, manifest scopes) lands.

	// AgentLLM is the language model backing conversation mode. A single
	// instance (the Anthropic key is workspace-independent), not a per-team
	// factory. Nil disables conversation mode.
	AgentLLM agent.LLM

	// AgentStore persists per-thread conversation history and Slack event-id
	// dedupe. Nil disables conversation mode.
	AgentStore *slackdata.AgentStore

	// PostMessage posts a chat.postMessage reply (threaded on threadTS) using
	// the per-workspace bot token, the same token seam as OpenView/PostDM.
	// Nil disables conversation mode.
	PostMessage PostMessageFunc

	// PostEphemeral posts a chat.postEphemeral message visible only to userID in a
	// channel/group conversation. The confirm flow delivers a get's one-time link this
	// way in a channel/private channel after the mpim boundary check: a STANDALONE
	// ephemeral, independent of the click's response_url, so the card-replace can't
	// overwrite it. (The 1:1-DM branch uses PostMessage instead — ephemerals don't
	// render in a DM.) Nil → the channel get delivery reports failure and the card
	// downgrades; it is NOT part of the agentEnabled gate.
	PostEphemeral PostEphemeralFunc

	// PostMarkdownMessage posts a chat.postMessage reply whose visible body is
	// standard Markdown rendered by Slack, rather than the text field's mrkdwn
	// dialect. It carries the agent's free-text answer so a channel reply renders
	// like the streaming pane, without a hand-rolled mrkdwn converter. Only the
	// agent's own answer routes here; an escaped proposal preview / error reply
	// stays on PostMessage (see deliverAgentResult). Nil falls back to PostMessage
	// (mrkdwn), so a turn still delivers even if this seam is unwired — it is NOT
	// part of the agentEnabled gate.
	PostMarkdownMessage PostMessageFunc

	// AgentDisabled is the org-level kill switch. True forces conversation mode
	// off regardless of the wiring above — the panic button independent of the
	// per-workspace toggle. Read from Config at construction, so flipping it is a
	// deploy-time action (redeploy/reconstruct), not a live runtime switch; a
	// hot-reloadable flag is deferred to the enablement work (see #651).
	AgentDisabled bool

	// PostMessageBlocks posts an interactive Block Kit message (chat.postMessage
	// with blocks) on the per-workspace bot token — the seam the conversation-mode
	// confirm flow uses to render a proposed mutation as an Approve/Reject card.
	// PostMessage (text-only) can't carry buttons. Nil keeps the confirm flow off
	// (see agentConfirmEnabled); production wires it in cmd/main.go.
	PostMessageBlocks PostMessageBlocksFunc

	// AgentConfirmEnabled gates the propose→confirm→execute flow on top of the
	// read-only conversation surface. While false (the default), a proposed
	// mutation is surfaced as today's text preview and nothing executes; while
	// true (and PostMessageBlocks is wired), the agent posts an interactive confirm
	// card and an Approve click executes the mutation after an independent admin
	// re-check. Separate from AgentDisabled/AgentLLM so the read-only surface can
	// ship enabled while the confirm flow stays staged (dark → beta).
	AgentConfirmEnabled bool

	// AgentChannelFollowups gates whether the agent answers non-@mention thread
	// replies in channel threads it already joined (a follow-up without a re-@mention;
	// see shouldDispatchAgentEvent). False (the default) keeps channels @mention-per-
	// turn. True requires the manifest to subscribe message.channels/groups with
	// channels:history/groups:history — so the bot then RECEIVES every message in
	// channels it's a member of (and only acts on its own threads). That's a
	// data-handling expansion: enable only after the review + a workspace re-OAuth.
	AgentChannelFollowups bool

	// AgentSurfaceExclusiveAcks switches pane (message.im) turns from the pre-pane
	// additive ack fallback (reaction + best-effort Debug setStatus) to the post-pane
	// exclusive ack path (native status only, Warn on setStatus failure). False by
	// default so deploying app code before the Slack manifest/pane smoke gate cannot
	// leave ordinary DMs indicator-less or Warn-spammy; flip true with the #1004
	// enablement once pane status behavior is confirmed. Exclusive mode assumes
	// AssistantThreads is wired, because there is intentionally no reaction fallback.
	AgentSurfaceExclusiveAcks bool

	// AgentDefaultEnabled is the per-workspace conversation-mode default for a
	// workspace that hasn't set the toggle (`/qurl-admin agent on|off`, stored in
	// workspace_mappings via AdminStore). False during the staged per-workspace
	// rollout (each workspace opts in); GA flips it true (every workspace on unless
	// it explicitly opted out). Read alongside the org-level agentEnabled gate — it
	// only matters once the org seams are wired and not killed.
	AgentDefaultEnabled bool

	// AgentMaxTurnsPerUserPerHour / AgentMaxTurnsPerTeamPerHour cap how many agent
	// turns a single member, and a whole workspace, can drive per rolling hour — a
	// cost backstop on LLM spend, enforced in processAgentEvent before the turn runs.
	// 0 disables that limit (unlimited). Both default to a conservative non-zero
	// value (see cmd/main.go) so a GA-live agent has a backstop even if the operator
	// never sets the env var. Unlike the workspace/dedupe gates these fail OPEN: a
	// transient counter error must not drop a legitimate turn.
	AgentMaxTurnsPerUserPerHour int
	AgentMaxTurnsPerTeamPerHour int

	// Reactions adds/removes the agent's glanceable "working on it" emoji on the
	// triggering message (reactions.add/remove on the per-workspace bot token). It is
	// a best-effort UX ack: a failure never fails the turn, and Nil simply omits the
	// ack (the reply posts exactly as before). Behind the kill switch via
	// agentEnabled() like the rest; production wires it in cmd/main.go.
	Reactions ReactionPort

	// ResolveChannelName resolves a channel id to its human name (conversations.info
	// on the per-workspace bot token, Grid-aware) so the agent's system prompt can
	// render "#general (C123)" instead of the bare id. Nil leaves
	// TurnContext.ChannelName empty — describeChannel falls back to the id — so the
	// agent works without the channels:read / groups:read scope. Results (including
	// failures) are cached per-Handler with a TTL, so a missing scope falls back to
	// the id for the TTL instead of re-hitting Slack every turn. Best-effort: a
	// resolve error never fails the turn.
	ResolveChannelName ResolveChannelNameFunc

	// ResolveConversationInfo resolves Slack conversation metadata for snapshot-less
	// confirm delivery decisions. Current cards use the snapshotted Events API
	// channel_type first, but legacy/snapshot-less G-prefixed gets can still use
	// is_mpim to avoid minting an access link into a group DM until that surface is
	// explicitly proven safe. Nil or lookup errors preserve the existing G-prefixed
	// ephemeral path so private-channel approvals do not regress in installs without
	// mpim:read; ordinary channels and 1:1 DMs do not need this lookup.
	ResolveConversationInfo ResolveConversationInfoFunc

	// ChannelMembership reports whether a user is a member of a channel
	// (conversations.members on the per-workspace bot token, Grid-aware). It gates whether
	// an assistant-pane turn may scope its reads to the channel the user opened the pane
	// from: only a confirmed member's pane is scoped, so the agent never enumerates a
	// channel's qURL topology to a non-member previewing it. Nil disables the scope (the
	// pane stays on the un-scoped DM). Best-effort + fail-closed: an error or timeout means
	// "not confirmed" → no scope. Results are cached per-Handler with a TTL. Reuses the
	// channels:read / groups:read scopes (same as ResolveChannelName).
	ChannelMembership ChannelMembershipFunc

	// AssistantThreads drives the Slack Assistants-container UX via assistant.threads.*:
	// setTitle / setSuggestedPrompts give a freshly-opened pane its first-run title +
	// starter prompts, and setStatus shows the native "thinking…" indicator while a pane
	// (DM) turn runs. Additive to the @mention/DM surface. Nil = no-op (the pane only
	// exists once the "Agents & AI Apps" manifest toggle + assistant:write scope are set),
	// so the surface stays dark until both the seam is wired and the manifest is updated.
	// Best-effort: a failure is logged, never surfaced.
	AssistantThreads AssistantThreadsPort

	// AppHomePublish publishes a user's App Home tab (views.publish) with the agent's
	// review surface — their own recent confirmed actions. Nil = no-op (the Home tab
	// only exists once the manifest's App Home feature + app_home_opened subscription
	// are enabled), so the surface stays dark until both the seam is wired and the
	// manifest is updated. Best-effort: a failure is logged, never surfaced.
	AppHomePublish AppHomePublishFunc

	// AgentStream drives Slack's native AI-app reply streaming (chat.startStream /
	// appendStream / stopStream) in the assistant pane, so a DM (pane) turn's reply
	// renders token-by-token instead of as one posted message. Nil (the default) keeps
	// the agent on the non-streaming post path; even when wired it only engages for a
	// pane turn and requires the assistant:write scope, so the surface stays dark until
	// the manifest enables it. Best-effort: any failure falls back to / leaves the
	// normal post path.
	AgentStream AgentStreamPort
}

// PostMessageFunc posts a Slack message via chat.postMessage on the
// per-workspace bot token. threadTS threads the reply (empty posts top-level).
// enterpriseID is passed for Enterprise Grid token resolution.
type PostMessageFunc func(ctx context.Context, teamID, enterpriseID, channelID, threadTS, text string) error

// PostDMFunc posts a direct message to slackUserID via chat.postMessage on the
// per-workspace bot token. enterpriseID is passed for Enterprise Grid token
// resolution, matching PostMessageFunc.
type PostDMFunc func(ctx context.Context, teamID, enterpriseID, slackUserID, text string) error

// PostEphemeralFunc posts a chat.postEphemeral message (visible only to userID) on the
// per-workspace bot token. threadTS threads it into the card's conversation (empty posts
// at channel root). Unlike a response_url ephemeral it's a standalone message, so a
// same-response_url card-replace can't clobber it. Returns an error on a non-ok response
// so the caller can downgrade the card.
type PostEphemeralFunc func(ctx context.Context, teamID, enterpriseID, channelID, threadTS, userID, text string) error

// PostMessageBlocksFunc posts an interactive Block Kit message via
// chat.postMessage on the per-workspace bot token. blocks is a slice of Block Kit
// block objects (the map[string]any shape the views.go builders emit, same as
// postResponseBlocks); fallbackText is the notification/accessibility text Slack
// shows where blocks can't render. threadTS threads the reply.
//
// Slack renders the top-level fallback text as mrkdwn by default, so a caller
// passing untrusted (e.g. LLM-distilled) text must escape it (the conversation
// confirm flow passes escapeMrkdwnText output); the production impl should also
// post with mrkdwn disabled as defense-in-depth.
type PostMessageBlocksFunc func(ctx context.Context, teamID, enterpriseID, channelID, threadTS string, blocks []any, fallbackText string) error

// AppHomePublishFunc publishes a user's App Home tab via views.publish on the
// per-workspace bot token (enterpriseID for Grid token resolution). blocks is the
// Home view's content (the map[string]any block shape the views.go builders emit); the
// impl wraps it in a {"type":"home"} view. The blocks already carry escaped echo (the
// caller renders untrusted action fields through escapeMrkdwn*), so no further
// sanitization is needed here.
type AppHomePublishFunc func(ctx context.Context, teamID, enterpriseID, userID string, blocks []any) error

// AgentStreamStart describes one chat.startStream call. TeamID/EnterpriseID select the
// bot token; RecipientTeamID/RecipientUserID identify the human Slack should deliver
// the streamed channel reply to. They are separate so Enterprise Grid/shared-channel
// turns can't accidentally send the installed workspace's team as the recipient team.
type AgentStreamStart struct {
	TeamID          string
	EnterpriseID    string
	ChannelID       string
	ThreadTS        string
	RecipientTeamID string
	RecipientUserID string
}

// AgentStreamPort drives Slack's native AI-app streaming over the per-workspace bot
// token (enterpriseID for Grid token resolution). StartStream opens a stream on a
// thread and returns the stream message's ts — the handle AppendStream/StopStream
// address. AppendStream appends a markdown chunk; StopStream finalizes the message.
// A nil port keeps the agent on the non-streaming post path; all three require the
// assistant:write scope.
type AgentStreamPort interface {
	StartStream(ctx context.Context, start *AgentStreamStart) (streamTS string, err error)
	AppendStream(ctx context.Context, teamID, enterpriseID, channelID, streamTS, markdownText string) error
	StopStream(ctx context.Context, teamID, enterpriseID, channelID, streamTS string) error
}

// ResolveChannelNameFunc resolves a channel id to its human name via
// conversations.info on the per-workspace bot token (enterpriseID for Grid token
// resolution). It returns an error on a missing scope, a DM/unknown channel, or a
// transport failure — the caller treats any error as "no name" and falls back to
// the channel id.
type ResolveChannelNameFunc func(ctx context.Context, teamID, enterpriseID, channelID string) (string, error)

// ConversationInfo is the small, Slack-sourced conversation metadata slice the
// handler needs beyond the human-readable name. Keep it intentionally narrow so
// conversations.info response growth does not become app surface area by accident.
type ConversationInfo struct {
	Name   string
	IsMPIM bool
}

// ResolveConversationInfoFunc resolves Slack conversation metadata via
// conversations.info on the per-workspace bot token (enterpriseID for Grid token
// resolution). It returns an error on a missing scope, unknown conversation, or
// transport/decode failure; callers choose whether that is best-effort or fail-closed.
type ResolveConversationInfoFunc func(ctx context.Context, teamID, enterpriseID, channelID string) (ConversationInfo, error)

// ChannelMembershipFunc reports whether userID is a member of channelID via
// conversations.members on the per-workspace bot token (enterpriseID for Grid token
// resolution). It returns (false, nil) when the bounded membership scan completes without
// finding the user — a non-member, or a member beyond the scanned bound — and a non-nil
// error on a transport/decode failure OR a Slack ok:false (missing scope, channel_not_found,
// or a private channel the bot isn't in). The caller treats any error or a false as "don't
// scope", so the gate is fail-closed by construction either way.
type ChannelMembershipFunc func(ctx context.Context, teamID, enterpriseID, channelID, userID string) (bool, error)

// SuggestedPrompt is one Assistants-container starter prompt: Title is the short
// clickable label, Message is the text inserted into the composer when clicked.
type SuggestedPrompt struct {
	Title   string
	Message string
}

// AssistantThreadsPort drives the Slack Assistants-container UX via the
// assistant.threads.* web API (per-workspace bot token, Grid-aware). channelID is
// the assistant DM channel, threadTS the thread the assistant_thread_started event
// opened. SetTitle and SetSuggestedPrompts are the first-run UX (set once when the
// pane opens); SetStatus shows the per-turn "thinking…" indicator while a turn runs,
// which Slack auto-clears when the agent posts its reply (an empty status also clears
// it). All are best-effort — the conversation surface still works without them.
type AssistantThreadsPort interface {
	SetTitle(ctx context.Context, teamID, enterpriseID, channelID, threadTS, title string) error
	SetSuggestedPrompts(ctx context.Context, teamID, enterpriseID, channelID, threadTS string, prompts []SuggestedPrompt) error
	SetStatus(ctx context.Context, teamID, enterpriseID, channelID, threadTS, status string) error
}

// ReactionPort adds and removes a single emoji reaction on a message via the Slack
// reactions.add / reactions.remove web API (per-workspace bot token, Grid-aware).
// name is the emoji short name without colons (e.g. "eyes"). timestamp is the target
// message's ts. The conversation surface uses it for a best-effort working-on-it ack,
// so implementations should treat the benign idempotent outcomes (add of an existing
// reaction, remove of an absent one) as success rather than errors.
type ReactionPort interface {
	Add(ctx context.Context, teamID, enterpriseID, channelID, timestamp, name string) error
	Remove(ctx context.Context, teamID, enterpriseID, channelID, timestamp, name string) error
}

// Handler processes Slack events and commands.
type Handler struct {
	cfg Config
	// now is injected so tests can pin the clock for timestamp-skew checks
	// without touching a package global. Defaults to time.Now.
	now func() time.Time
	// channelNames memoizes agent channel-name resolutions (cfg.ResolveChannelName)
	// for the process with a TTL. nil-receiver-safe, so a Handler built without
	// NewHandler simply doesn't cache. See handler_agent_channel.go.
	channelNames *channelNameCache
	// channelMembers memoizes agent (channel, user) membership decisions
	// (cfg.ChannelMembership) for the process with a TTL. nil-receiver-safe; see
	// handler_agent_membership.go.
	channelMembers *channelMembershipCache
	// oauthSetup carries the runtime configuration the /qurl setup
	// slash-command needs to mint a state token and build the /start
	// URL. nil when the OAuth surface is not configured (sandbox /
	// missing env vars) — /qurl setup returns a "not configured"
	// ephemeral in that case rather than minting a useless link.
	oauthSetup *oauth.SetupConfig
	// aliasStore persists per-channel alias bindings for the
	// `/qurl-admin set-alias` / `/qurl-admin unset-alias` verbs. nil when not
	// configured (sandbox / pre-#231/#233 deploys) — handlers fail
	// fast with an operator-visible ephemeral rather than silently
	// dropping the write. See handler_alias.go for the interface
	// shape and the schema-gap rationale.
	aliasStore AliasStore
	// baseCtx is captured at NewHandler time from cfg.BaseContext (or
	// context.Background()). Each async goroutine derives a
	// context.WithTimeout(baseCtx, asyncWorkTimeout) — canceling baseCtx
	// at HTTP drain start (after main.go's lameduck) signals every
	// in-flight worker.
	baseCtx context.Context
	// unhealthy flips /health to 503 while the task is in lameduck.
	// It is updated by the shutdown goroutine while ALB health probes
	// can still be in flight, so the flag must be safe on the request
	// hot path.
	unhealthy atomic.Bool
	// agentAckTimeout is captured from Config.AgentAckTimeout once at construction.
	// The handler reads it without synchronization on the request hot path.
	agentAckTimeout time.Duration
	// wg tracks live async workers so cmd/main.go's Wait() can drain
	// them after http.Server.Shutdown returns. wg.Add MUST happen on
	// the request goroutine (before the `go` keyword) — adding inside
	// the spawned goroutine races Wait().
	wg sync.WaitGroup
	// activeWorkers mirrors wg for the zero-budget shutdown path: it lets
	// WaitTimeout(0) distinguish "nothing left to drain" from "workers
	// still pending" without racing an immediate timer.
	activeWorkers atomic.Int64
	// sem is a buffered-channel semaphore bounding concurrent async
	// workers to len(sem) capacity. Send-with-default-drop gives back-
	// pressure feedback to the user as ackBusy rather than queueing.
	sem chan struct{}
	// followupSem is the SEPARATE bounded pool for channel thread follow-ups (#712).
	// Routing the message.channels firehose here keeps a busy channel's chatter from
	// saturating h.sem, which @mention/DM/slash/interaction work shares. It is held only
	// after the short followupGateSem admission check passes, and the combined pipeline is
	// wg-tracked so shutdown drains it.
	followupSem chan struct{}
	// followupGateSem bounds the short "is this thread ours?" DDB gate before an
	// admitted channel follow-up takes a long-running followupSem slot (#719).
	followupGateSem chan struct{}
	// responseURLClient is owned per-Handler so tests can inject a
	// transport and so the lifetime is tied to the handler (not the
	// per-request goroutine).
	responseURLClient *http.Client
	// validateResponseURLFn defaults to validateResponseURL — pinned to
	// https://hooks.slack.com/* in production. Tests override it to
	// permit httptest server URLs (which are http://127.0.0.1:NNNNN).
	// Field rather than parameter so the production default needs no
	// per-deploy wiring.
	//
	// Returns a *url.URL on success rather than just an error so the
	// caller dials the validated/reconstructed URL — the production
	// validator pins Scheme and Host to literal constants on the
	// returned value, which is the SSRF-sanitization pattern CodeQL's
	// taint analysis recognizes.
	validateResponseURLFn func(string) (*url.URL, error)
}

// SetAliasStore wires the per-channel alias persistence surface into
// the /qurl-admin set-alias / /qurl-admin unset-alias verbs. Must be called before
// `srv.Serve` — the field is read on the request hot path without
// synchronization, and the only safe write window is before any
// goroutine can observe it. The panic-on-double-wiring below catches
// accidental double-`SetAliasStore(realStore)` in init code; it is
// NOT a synchronization primitive, and calling this from a running
// handler is undefined regardless of the panic.
//
// Calling with nil is a no-op for the field (the verbs will reply
// with a "not configured" ephemeral) so cmd/main.go can omit the
// call on sandbox deploys that haven't onboarded the slackdata
// package yet. Both directions of the nil/non-nil sequence are
// allowed: a defensive `SetAliasStore(nil)` followed by a real
// wiring later is fine, and a real wiring followed by a defensive
// `SetAliasStore(nil)` is also a no-op (the real store stays wired).
// Calling with a non-nil store after the field is already non-nil
// panics, so the real store can't be silently swapped under a
// running handler.
func (h *Handler) SetAliasStore(store AliasStore) {
	if store == nil {
		return
	}
	if h.aliasStore != nil {
		panic("SetAliasStore called twice with a non-nil store — must be wired once before Serve")
	}
	h.aliasStore = store
}

// SetOAuthSetup wires the per-workspace OAuth configuration into the
// /qurl setup slash command. Must be called exactly once, before
// srv.Serve. Empty/short secret or empty base URL is a no-op
// (/qurl setup will reply that OAuth is not configured). A second call
// panics — the field is read without synchronization on the request
// hot path, and the only safe write window is before any goroutine can
// observe it.
func (h *Handler) SetOAuthSetup(cfg oauth.SetupConfig) {
	if h.oauthSetup != nil {
		panic("SetOAuthSetup called twice — must be called once before Serve")
	}
	if len(cfg.StateSecret) == 0 || cfg.SlackBaseURL == "" {
		return
	}
	if len(cfg.StateSecret) < oauth.StateMinSecret {
		// Fail-fast at startup: MintState would reject this later, but
		// the operator-facing failure is more discoverable here.
		panic("SetOAuthSetup: StateSecret shorter than oauth.StateMinSecret")
	}
	// Defensive copy: the field is read on the request hot path without
	// a lock. A caller mutating the original byte slice would silently
	// poison every subsequent MintState call.
	cfg.StateSecret = append([]byte(nil), cfg.StateSecret...)
	h.oauthSetup = &cfg
}

// SetSlackInstallURL wires the customer Slack install URL used to recover
// guided tunnel setup when a workspace has no stored bot token yet. Must be
// called before Serve. Empty is a no-op so deployments without Slack install
// OAuth keep the operator-directed fallback copy.
func (h *Handler) SetSlackInstallURL(installURL string) {
	installURL = strings.TrimSpace(installURL)
	if installURL == "" {
		return
	}
	h.cfg.SlackInstallURL = installURL
}

// SetHealthy controls the task-level /health response. Production flips
// this false during SIGTERM lameduck so ALB stops routing to the task
// before the listener closes; tests may flip it back to true to exercise
// recovery of the endpoint contract.
func (h *Handler) SetHealthy(healthy bool) {
	h.unhealthy.Store(!healthy)
}

// NewHandler creates a new Slack handler. Config is intentionally
// passed by value rather than pointer despite gocritic's hugeParam
// warning: the call site is once at process startup (cmd/main.go)
// or once per t.Run in tests, so the copy is amortized to zero
// against the bot's lifetime. Pass-by-value keeps callers from
// mutating fields out from under the handler.
//
//nolint:gocritic // hugeParam: Config copied once per Handler at startup; pass-by-value is intentional.
func NewHandler(cfg Config) *Handler {
	baseCtx := cfg.BaseContext
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	maxAsync := cfg.MaxConcurrentAsync
	if maxAsync <= 0 {
		maxAsync = defaultMaxConcurrentAsync
	}
	maxFollowupAsync := cfg.MaxConcurrentFollowupAsync
	if maxFollowupAsync <= 0 {
		maxFollowupAsync = defaultMaxConcurrentFollowupAsync
	}
	maxFollowupGateAsync := cfg.MaxConcurrentFollowupGateAsync
	if maxFollowupGateAsync <= 0 {
		maxFollowupGateAsync = defaultMaxConcurrentFollowupGateAsync
	}
	agentAckTimeout := cfg.AgentAckTimeout
	if agentAckTimeout <= 0 {
		agentAckTimeout = defaultAgentAckTimeout
	}
	respClient := cfg.ResponseURLClient
	if respClient == nil {
		respClient = defaultResponseURLClient()
	}
	return &Handler{
		cfg:                   cfg,
		now:                   time.Now,
		baseCtx:               baseCtx,
		agentAckTimeout:       agentAckTimeout,
		sem:                   make(chan struct{}, maxAsync),
		followupSem:           make(chan struct{}, maxFollowupAsync),
		followupGateSem:       make(chan struct{}, maxFollowupGateAsync),
		responseURLClient:     respClient,
		validateResponseURLFn: validateResponseURL,
		channelNames:          newChannelNameCache(channelNameTTL),
		channelMembers:        newChannelMembershipCache(channelMembershipTTL),
	}
}

// Wait blocks until every async worker spawned by this handler has
// returned. Call it after http.Server.Shutdown so the process doesn't
// exit while a goroutine is still mid-POST to Slack's response_url.
//
// Wait is a no-op if no async work is in flight, so the cmd path can
// always call it on every shutdown without conditionals.
//
// In production, prefer WaitTimeout — an unbounded Wait() leaves the
// process exposed to a future regression where a worker ignores its
// ctx, wedging shutdown past the platform's hard-kill window.
func (h *Handler) Wait() {
	h.wg.Wait()
}

func (h *Handler) asyncStart() {
	h.activeWorkers.Add(1)
	h.wg.Add(1)
}

func (h *Handler) asyncDone() {
	h.wg.Done()
	h.activeWorkers.Add(-1)
}

// Compile-time check that *Handler still satisfies oauth.AsyncTracker
// after any future rename of Handler.Go — would break here rather
// than nil-tracker the OAuth callback's fire-and-forget revoke path
// at runtime.
var _ oauth.AsyncTracker = (*Handler)(nil)

// Go runs fn in a goroutine tracked by h.wg so the cmd shutdown drain
// covers it. Implements oauth.AsyncTracker — the OAuth callback's
// fire-and-forget DM and orphan-key revoke goroutines flow through
// here, putting them inside the same WaitTimeout budget as the
// slash-command async workers.
//
// Panics in fn are recovered with a stack-trace log so a buggy Slack
// client or qurl-service stub can't crash the bot. Mirrors the
// recover discipline in runAsync.
func (h *Handler) Go(fn func()) {
	h.asyncStart()
	go func() {
		defer h.asyncDone()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in tracked async goroutine",
					"recover", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// WaitTimeout drains in-flight async workers, returning early after d.
// Returns true on clean drain; false on timeout (workers still in
// flight). cmd/main.go uses this so a misbehaving worker can't block
// graceful shutdown past the SIGTERM→SIGKILL window.
//
// Note: on the timeout path the inner h.wg.Wait goroutine outlives
// this call until the underlying workers actually finish. This is fine
// in the cmd shutdown path (the process is exiting) but means
// WaitTimeout is NOT appropriate as a hot-path drain primitive — only
// use at end-of-life.
func (h *Handler) WaitTimeout(d time.Duration) bool {
	if d <= 0 {
		return h.activeWorkers.Load() == 0
	}

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// defaultResponseURLClient is the http.Client used to POST follow-up
// messages to Slack's response_url unless the caller injects one.
//
// CheckRedirect refusing redirects is load-bearing for the
// host-pinning posture: validateResponseURL only validates the URL the
// caller provided, so a 30x bounce from hooks.slack.com to any host
// would otherwise be silently followed (Go's default cap is 10 hops).
// Returning ErrUseLastResponse surfaces the 30x to the caller without
// dialing the redirected target.
func defaultResponseURLClient() *http.Client {
	return &http.Client{
		Timeout: responseURLTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health checks are silent: ALB target-group probes hit this every
	// 15-30s per task and would otherwise dominate log volume.
	if r.URL.Path == pathHealth {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if h.unhealthy.Load() {
				respondJSON(w, http.StatusServiceUnavailable, map[string]string{healthStatusKey: healthStatusDraining})
				return
			}
			respondJSON(w, http.StatusOK, map[string]string{healthStatusKey: healthStatusOK})
		default:
			respondMethodNotAllowed(w, "GET, HEAD")
		}
		return
	}

	// Exact-path match by design: Slack sends the canonical paths without
	// trailing slashes, and a strict match means a path-rewriting proxy
	// can't accidentally normalize "/slack/commands/" into a 404 silently
	// further upstream — it dies here in our routing instead. If we ever
	// front this with such a proxy, switch to strings.TrimRight or move
	// to http.ServeMux.
	switch r.URL.Path {
	case pathSlackCommands, pathSlackEvents, pathSlackInteractions:
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, "POST")
			return
		}
	default:
		// Silent on 404 — and 405 on /slack/* takes the same path
		// (method gate above returns before the request log fires).
		// ALB target groups are reachable to internet probes
		// (/wp-login.php, /.env, credentialed scrapers GET-ing
		// /slack/commands); logging each would be noise. Slack and
		// health paths are the only legitimate surface and they get
		// their own log lines.
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	slog.Info("received request", "path", r.URL.Path, "method", r.Method) //nolint:gosec // G706: slog's JSON handler escapes control chars in attribute values, so tainted paths can't inject log lines.

	// Honest oversize declarations get rejected before allocation.
	// MaxBytesReader still catches dishonest senders during the read.
	if r.ContentLength > maxRequestBodyBytes {
		slog.Info("oversize body rejected", "path", r.URL.Path, "reason", "content_length_pre_check", "declared", r.ContentLength) //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
		respondPayloadTooLarge(w)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		// Same operational condition as the Content-Length pre-check
		// above; bucket them together so dashboards see one 413 stream.
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			slog.Info("oversize body rejected", "path", r.URL.Path, "reason", "max_bytes_during_read") //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
			respondPayloadTooLarge(w)
			return
		}
		slog.Warn("failed to read request body", "error", err, "path", r.URL.Path) //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	if err := h.verifySlackRequest(r, body); err != nil {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature verification failed"})
		return
	}

	switch r.URL.Path {
	case pathSlackCommands:
		// r.Context() is intentionally NOT threaded into the slash-command
		// dispatch: handleGet/handleListResources spawn goroutines that outlive
		// the HTTP response, and r.Context() cancels as soon as ServeHTTP
		// returns. Async work uses h.baseCtx instead.
		h.handleSlashCommand(w, body)
	case pathSlackEvents:
		h.handleEvent(w, body)
	case pathSlackInteractions:
		h.handleInteraction(w, body)
	}
}

// readBody reads the full request body up to maxRequestBodyBytes. Slack
// signature verification needs the exact bytes, and the parsed body is
// everything the downstream handlers need — the body is consumed here.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
}

// verifySlackRequest authenticates a request against the configured
// signing secret. Side-effect-free aside from a slog line on failure.
func (h *Handler) verifySlackRequest(r *http.Request, body []byte) error {
	sig := r.Header.Get(headerSlackSignature)
	ts := r.Header.Get(headerSlackTimestamp)
	err := verifySlackSignature(h.cfg.SlackSigningSecret, body, sig, ts, h.now())
	if err != nil {
		attrs := []any{
			"path", r.URL.Path,
			"reason", classifySlackErr(err),
			"has_signature", sig != "",
			"has_timestamp", ts != "",
		}
		// Empty secret means the deployment is effectively open — page on
		// it distinctly from ordinary 401 noise.
		if errors.Is(err, errSlackSigningSecretEmpty) {
			slog.Error("slack signature verification failed — signing secret is empty (deployment is open)", attrs...) //nolint:gosec // G706: attrs carries r.URL.Path which slog escapes.
		} else {
			slog.Warn("slack signature verification failed", attrs...) //nolint:gosec // G706: attrs carries r.URL.Path which slog escapes.
		}
	}
	return err
}

// classifySlackErr maps the sentinel verification errors to stable metric
// labels so operator dashboards can group by cause without string-matching
// error messages. "secret_empty" is unreachable under normal startup —
// cmd/main.go refuses to boot without SLACK_SIGNING_SECRET — so seeing
// it in telemetry implies a code path that bypassed the main entry point
// (tests, custom runtime, etc.).
func classifySlackErr(err error) string {
	switch {
	case errors.Is(err, errSlackSigningSecretEmpty):
		return "secret_empty"
	case errors.Is(err, errSlackSignatureMissing):
		return "headers_missing"
	case errors.Is(err, errSlackSignatureMalformed):
		return "sig_malformed"
	case errors.Is(err, errSlackTimestampMalformed):
		return "ts_malformed"
	case errors.Is(err, errSlackTimestampStale):
		return "stale"
	case errors.Is(err, errSlackSignatureMismatch):
		return "mismatch"
	default:
		return "unknown"
	}
}

func slashSubcommand(text, command string) bool {
	matched, _ := slashVerb(text, command)
	return matched
}

// adminVerbs are the leading verb words that belong to `/qurl-admin`.
const (
	adminVerbProtect = "protect"
	// protect-connector / protect-url are the single-word connector/URL verbs.
	// Bare (no arguments) opens the matching guided modal; with arguments they
	// are the typed power-user forms. They replaced the former two-word
	// connector/URL grammar — the slash surface is
	// single word or hyphenated-word only, no space-separated sub-verbs.
	adminVerbProtectConnector = "protect-connector"
	adminVerbProtectURL       = "protect-url"
	// adminVerbAgent is `/qurl-admin agent on|off` — the per-workspace
	// conversation-mode toggle (bare `agent` shows the current state).
	adminVerbAgent = "agent"
)

// Used to redirect a user who typed an admin verb on `/qurl` and to
// classify the wrong-surface case. `set-alias`/`unset-alias` carry both
// spellings because slashVerb accepts the dash-free historical form too.
// `add`/`remove`/`admins`/`revoke` are the flat membership + revoke verbs;
// `admin` is retained only so the deprecated `admin <verb>` prefix still
// classifies here (it gets a redirect in dispatchAdminCommand). `setup` is
// deliberately NOT here — it lives on `/qurl` (see handleSetup) so the first
// claimant of an unbound workspace can reach it.
//
// Adding an admin verb touches three places that must stay in sync: this
// list (wrong-surface classification), a dispatch case in
// dispatchAdminCommand, and — if it's user-facing — adminHelpMessage.
//
// Immutable: read-only on the request hot path (slashVerb ranges it); a
// var only because Go has no const slice. Do not mutate at runtime.
var adminVerbs = []string{string(SubcmdAdmin), adminVerbProtect, adminVerbProtectConnector, adminVerbProtectURL, adminVerbAgent, "set-alias", string(SubcmdSetAlias), "unset-alias", string(SubcmdUnsetAlias), "set-display-name", "unset-display-name", "add", "remove", "admins", "revoke"}

// userVerbs are the leading verb words that belong to `/qurl`. Used to
// redirect a user who typed a user verb on `/qurl-admin`. `setup` is a
// user verb (first-come-claims; see handleSetup), so `/qurl-admin setup`
// redirects here to `/qurl setup`. Immutable like adminVerbs (see above).
var userVerbs = []string{"get", "list", "aliases", "create", "setup", uninstallVerb, "feedback"}

// isAdminVerb reports whether text's leading verb is an admin verb.
func isAdminVerb(text string) bool {
	matched, _ := slashVerb(text, adminVerbs...)
	return matched
}

// isUserVerb reports whether text's leading verb is a user verb.
func isUserVerb(text string) bool {
	matched, _ := slashVerb(text, userVerbs...)
	return matched
}

// firstWord returns the first whitespace-delimited token of text, or ""
// when text has none. The wrong-surface redirects echo it as the verb
// word — `admin <action>` collapses to `admin` so the redirect reads
// `/qurl-admin admin list`, matching the retained sub-word grammar. It's
// reached only on already-classified verb text, so the token is a
// known-literal keyword; the redirects echoText-wrap it anyway to keep the
// inline-code-fence safety local rather than by chained invariant.
func firstWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// stripBackticks removes backticks from user-controlled text echoed into a
// Slack inline-code span (the wrong-surface and unknown-subcommand replies).
// A stray backtick in the echoed text would otherwise unbalance the `…` fence
// and render the ephemeral reply garbled. Rendering hygiene, not a security
// boundary — ephemerals are plain text, not markup-trusted.
func stripBackticks(s string) string {
	return strings.ReplaceAll(s, "`", "")
}

// maxEchoRunes caps how much user-typed command text the wrong-surface and
// unknown-subcommand replies echo back. The echo is a copy-paste convenience,
// not data; an unbounded paste would render an ungainly ephemeral (Slack also
// truncates server-side, but at a less predictable point). 200 runes
// comfortably fits any real command invocation.
const maxEchoRunes = 200

// echoText prepares user-controlled command text for echoing into a Slack
// inline-code span: it strips backticks (so a stray one can't unbalance the
// `…` fence) and caps the length (so an oversized paste renders predictably).
func echoText(s string) string {
	s = stripBackticks(s)
	// Byte length is an upper bound on rune count, so a string within the cap
	// by bytes is within it by runes too — skip the []rune conversion in the
	// common short case.
	if len(s) <= maxEchoRunes {
		return s
	}
	if r := []rune(s); len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	// Byte length exceeded the cap but rune count didn't (multi-byte text):
	// within budget by runes, so return as-is rather than truncate.
	return s
}

func slashVerb(text string, verbs ...string) (matched bool, rest string) {
	for _, verb := range verbs {
		if text == verb {
			return true, ""
		}
		if strings.HasPrefix(text, verb+" ") {
			return true, strings.TrimSpace(strings.TrimPrefix(text, verb))
		}
	}
	return false, text
}

func setAliasSubcommand(text string) bool {
	matched, _ := slashVerb(text, "setalias", "set-alias")
	return matched
}

func stripSetAliasPrefix(text string) string {
	_, rest := slashVerb(text, "setalias", "set-alias")
	return rest
}

func unsetAliasSubcommand(text string) bool {
	matched, _ := slashVerb(text, "unsetalias", "unset-alias")
	return matched
}

func stripUnsetAliasPrefix(text string) string {
	_, rest := slashVerb(text, "unsetalias", "unset-alias")
	return rest
}

func stripSetDisplayNamePrefix(text string) string {
	_, rest := slashVerb(text, "set-display-name")
	return rest
}

func stripUnsetDisplayNamePrefix(text string) string {
	_, rest := slashVerb(text, "unset-display-name")
	return rest
}

func (h *Handler) handleSlashCommand(w http.ResponseWriter, body []byte) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
		return
	}

	command := values.Get(fieldCommand)
	text := strings.TrimSpace(values.Get(fieldText))
	// Parse before dispatch so setup emails are redacted even when a user types
	// the setup verb on the admin slash-command surface.
	setupCmd, setupMatched, setupErr := parseSetupSubcommand(text)

	logText := text
	if setupMatched && text != setupVerb {
		logText = "setup <email>"
	}
	slog.Info("slash command", "command", command, "text", logText)

	// Normalize an empty command (malformed/synthetic payload) to the prod
	// user literal once, here at the HTTP entry, so dispatch, help, and the
	// wrong-surface redirect copy downstream never have to guard for it.
	// Empty already routes to the user surface below (isAdminCommand("") is
	// false), so this only fixes the rendered command name, not the routing.
	if command == "" {
		command = commandUser
	}

	// Both the user command and the admin command POST to the same request
	// endpoint with the same HMAC gate; Slack stamps which one was invoked
	// in the `command` field. Branch on it FIRST so the user surface and
	// the admin surface stay cleanly separated: a verb typed on the wrong
	// command gets a "that's a user command" / "that's an admin command"
	// redirect rather than the bare "unknown subcommand" reply.
	//
	// Classification is by the `-admin` suffix (isAdminCommand), not the
	// literal commandAdmin, so a non-prod env whose commands carry an infix
	// (`/qurl-sandbox`, `/qurl-sandbox-admin`) routes admin verbs to the
	// admin surface too. Matching the literal `/qurl-admin` would send
	// `/qurl-sandbox-admin` down the user path, making every admin verb
	// unreachable in that env.
	if isAdminCommand(command) {
		h.dispatchAdminCommand(w, command, text, values)
	} else {
		// The user command and any unrecognized command land here.
		// Unrecognized is defensive — Slack only sends the commands the app
		// registers — and the user surface is the safe default (it never
		// mutates admin state). Cosmetic caveat: help/redirect copy echoes
		// the invoked command name, so an unrecognized `command` (e.g.
		// `/qurl-bogus`) would render help advertising `/qurl-bogus list`
		// etc. — names that don't exist. We can't distinguish a bogus
		// command from a valid non-prod env command (`/qurl-sandbox`)
		// without a registry, so this is left as-is; it's unreachable given
		// Slack only dispatches registered commands.
		h.dispatchUserCommand(w, command, text, values, setupCmd, setupMatched, setupErr)
	}
}

// dispatchUserCommand routes the user-facing `/qurl` verbs: setup, get,
// list, aliases, feedback, help. Admin verbs typed on `/qurl` get a redirect to
// `/qurl-admin` instead of the generic unknown-subcommand reply, so an
// admin who fat-fingers the command gets a direct correction.
func (h *Handler) dispatchUserCommand(w http.ResponseWriter, command, text string, values url.Values, setupCmd setupCommand, setupMatched bool, setupErr error) {
	switch {
	case text == "" || text == "help":
		respondSlack(w, h.userHelpMessage(command))
	case setupMatched:
		if setupErr != nil {
			if errors.Is(setupErr, errSetupUsage) {
				respondSlack(w, fmt.Sprintf("Usage: `%s setup <email> [--rotate|--repoint]`.", command))
				return
			}
			respondSlack(w, fmt.Sprintf("That doesn't look like a valid email address. Use `%s setup <email> [--rotate|--repoint]`.", command))
			return
		}
		// setup is a `/qurl` verb, not admin-gated — first-come-claims;
		// see handleSetup for why it lives on the open user surface.
		h.handleSetup(w, values, setupCmd)
	case text == uninstallVerb:
		// uninstall stays on the user command as a lifecycle sibling of setup,
		// but gates to the recorded workspace owner or explicit qURL admins
		// before removing the key.
		h.handleUninstall(w, values)
	case slashSubcommand(text, uninstallVerb):
		// Bare `/qurl uninstall` is handled above so unsupported deployments can
		// explain that operator-managed installs are not self-service. Argument
		// variants land here: advertise usage when uninstall is live, otherwise
		// keep the same unsupported-deployment message as the bare verb. This
		// deliberately pays the owner/admin read before printing Usage so
		// unauthorized callers do not get a command-existence oracle.
		if _, _, ok := h.requireUninstallAvailableAndAuthorized(w, values); !ok {
			return
		}
		respondSlack(w, fmt.Sprintf("Usage: `%s uninstall`.", command))
	case slashSubcommand(text, "create"):
		// `/qurl create` is deprecated. It minted for an arbitrary URL,
		// which Slack no longer does — `/qurl get` mints for a tunnel
		// `$slug` or a channel `$alias`. Surface a deprecation hint
		// instead of an "unknown subcommand" so existing users hitting
		// muscle memory get a direct redirect to the new shape.
		respondSlack(w, "`/qurl create` is no longer supported. Use `/qurl get <$id|$alias>` instead — run `/qurl list` to see your resources.")
	case text == "list":
		// Exact match only: the looser `HasPrefix(text, "list")` form
		// matched `listing`, `lists`, `list-foo` (silently routing
		// them to the list handler) AND `list extra args` (which
		// processListResources ignores). Anything other than the
		// bare token falls through to the unknown-subcommand branch
		// and gets a help nudge.
		h.handleListResources(w, values)
	case slashSubcommand(text, "get"):
		// Exact-token boundary so `getter`, `get-foo` fall through
		// to the unknown-subcommand branch instead of silently
		// routing here. The parser then produces ErrEmptyResource
		// for a bare `get`.
		h.handleGet(w, values)
	case text == "aliases":
		h.handleAliases(w, values)
	case slashSubcommand(text, "feedback"):
		// feedback is a user verb available to any workspace member — no
		// admin gate, no qURL setup required (it never calls qurl-service).
		// slashSubcommand (not exact match) so `feedback <stray text>` still
		// opens the form rather than falling through to "unknown subcommand".
		h.handleFeedback(w, values)
	case isAdminVerb(text):
		// An admin verb typed on `/qurl` — redirect to `/qurl-admin` rather
		// than the generic unknown reply. firstWord(text) is the classified
		// verb word; the full text is echoed (backticks stripped) so the
		// correction is copy-pasteable without a stray backtick unbalancing
		// the inline-code span in the ephemeral reply.
		adminCmd := adminCommandName(command)
		respondSlack(w, fmt.Sprintf("`%s` is an admin command. Use `%s %s` instead, or run `%s help`.", echoText(firstWord(text)), adminCmd, echoText(text), adminCmd))
	default:
		// Surfaced to telemetry so a workspace using a stale slash-command
		// spec is visible in dashboards (rather than only via user reports).
		slog.Info("unknown slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `%s help`.", echoText(text), command))
	}
}

// dispatchAdminCommand routes the admin-facing `/qurl-admin` verbs:
// tunnel install, set-alias, unset-alias, set-display-name,
// unset-display-name, the flat membership verbs add/remove/admins, revoke,
// and help. User verbs typed on `/qurl-admin` — including `setup`, which is a
// `/qurl` verb (first-come-claims; see handleSetup) — get a redirect to
// `/qurl` so a user who fat-fingers the command gets a direct correction.
//
// The membership verbs are flat (`/qurl-admin add @user`, not `admin add`):
// the whole command is already admin-scoped, so the `admin` sub-word was
// redundant. Listing admins is `admins` (a plural noun) rather than `list` so
// it doesn't collide with `/qurl list` (which lists resources). The legacy
// `admin <verb>` prefix gets a one-line redirect to the flat form below.
func (h *Handler) dispatchAdminCommand(w http.ResponseWriter, command, text string, values url.Values) {
	// Verb-match order is defensive, not load-bearing today: the
	// membership/tunnel/alias matches come before the isUserVerb fall-through.
	// slashVerb requires an exact token or a `verb ` prefix, so `admins`
	// doesn't match `admin` (needs exact or `admin ` prefix) and neither
	// matches the user verb `list` regardless of order. Keeping admin matches
	// first guards against a FUTURE user verb that would collide as the
	// leading token of an admin sub-word grammar.
	switch {
	case text == "" || text == "help":
		// help is intentionally NOT admin-gated, unlike every verb below it:
		// it's discovery, not a privileged action. Gating it would obscure
		// (a non-admin couldn't learn what exists) rather than protect — the
		// roster it renders is the same public grammar carried in the user
		// surface's `/qurl-admin help` pointer. The actual admin verbs each
		// gate in their own handler (requireAdminSync).
		respondSlack(w, h.adminHelpMessage(command))
	case slashSubcommand(text, "revoke"):
		// Revoke a protected resource AND all its qURLs by `$<id|alias>`.
		// Resource-scoped — replaces the former `admin revoke <qurl_id>`
		// per-link kill. Runs async (multi-hop resolve+delete); see
		// handleRevoke.
		h.handleRevoke(w, values)
	case slashSubcommand(text, "add"), slashSubcommand(text, "remove"), slashSubcommand(text, "admins"):
		// Flat bot-admin membership verbs. handleAdmin parses the flat form
		// (Parse maps add/remove/admins → SubcmdAdmin + AdminAction) and gates
		// each in its own handler (requireAdminSync). Bare `add`/`remove`
		// surface ErrMissingUserMention; `admins` takes no args.
		h.handleAdmin(w, values)
	case slashSubcommand(text, "admin"):
		// Deprecated `admin <verb>` prefix — the word is redundant on an
		// already-admin command. Redirect to the flat verbs rather than
		// silently accepting it, so muscle-memory users learn the new grammar.
		respondSlack(w, fmt.Sprintf("The `admin` prefix isn't needed anymore — use `%[1]s add @user`, `%[1]s remove @user`, `%[1]s admins`, or `%[1]s revoke $<id>` directly.", command))
	// protect-connector / protect-url precede the bare `protect` chooser. slashVerb
	// matches an exact token or a `verb ` (space) prefix, so `protect` can't
	// shadow the hyphenated verbs regardless of order; the adjacency is for
	// readability — all three protect entries sit together. Each connector/URL
	// verb opens its guided modal when bare and runs the typed power-user form
	// when given arguments (handleExposeConnector / handleExposeURL).
	case slashSubcommand(text, adminVerbAgent):
		// Per-workspace conversation-mode toggle: `agent on|off` (bare shows state).
		h.handleAgentToggle(w, values)
	case slashSubcommand(text, adminVerbProtectConnector):
		h.handleExposeConnector(w, values)
	case slashSubcommand(text, adminVerbProtectURL):
		h.handleExposeURL(w, values)
	case slashSubcommand(text, adminVerbProtect):
		h.handleExpose(w, values)
	case setAliasSubcommand(text):
		// Bare `set-alias` falls through too — parseAliasArgs renders
		// the usage hint, so the user gets the right grammar without
		// a separate "missing args" branch here.
		h.handleSetAlias(w, values)
	case unsetAliasSubcommand(text):
		h.handleUnsetAlias(w, values)
	// Use slashSubcommand directly here (unlike set-alias's dedicated
	// helper): the verb has a single canonical spelling, and the
	// cross-repo dispatcher-drift check (qurl-integrations-infra) only
	// extracts the slashSubcommand and …AliasSubcommand case shapes — a
	// …DisplayNameSubcommand helper would be invisible to it and keep the
	// infra manifest drift check red even after this merges.
	case slashSubcommand(text, "set-display-name"):
		// A bare `set-display-name` (no args) matches this arm too; the
		// handler then renders the usage hint, so the user gets the right
		// grammar without a separate "missing args" branch here.
		h.handleSetDisplayName(w, values)
	case slashSubcommand(text, "unset-display-name"):
		h.handleUnsetDisplayName(w, values)
	case isUserVerb(text):
		// A user verb typed on the admin command — redirect to the user one.
		// Echoed text has backticks stripped (see the /qurl-side redirect).
		userCmd := userCommandName(command)
		suggestedText := echoText(text)
		if text == setupVerb {
			suggestedText = setupVerb + " <email>"
		}
		respondSlack(w, fmt.Sprintf("`%s` belongs on `%s`. Use `%s %s` instead, or run `%s help`.", echoText(firstWord(text)), userCmd, userCmd, suggestedText, userCmd))
	default:
		slog.Info("unknown admin slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown admin subcommand: `%s`. Try `%s help`.", echoText(text), command))
	}
}

func parseSetupSubcommand(text string) (setupCommand, bool, error) {
	rest, matched := setupVerbRest(text)
	if !matched {
		return setupCommand{}, false, nil
	}
	// strings.Fields("") returns an empty slice, so the bare-setup case folds
	// into the len check — same errSetupUsage either way.
	parts := strings.Fields(rest)
	if len(parts) == 0 || len(parts) > 2 {
		return setupCommand{}, true, errSetupUsage
	}
	cmd := setupCommand{mode: oauth.SetupModeReuse}
	seenMode := false
	for _, part := range parts {
		switch part {
		case setupFlagRotate, setupFlagRepoint:
			if seenMode {
				return setupCommand{}, true, errSetupUsage
			}
			seenMode = true
			// --rotate is an explicit same-account key replacement. --repoint
			// additionally handles the cross-account move: it resolves to a
			// rotation when the signed-in qURL account already holds the key and
			// otherwise routes the owner to the operator-assisted transfer.
			if part == setupFlagRepoint {
				cmd.mode = oauth.SetupModeRepoint
			} else {
				cmd.mode = oauth.SetupModeRotate
			}
		default:
			if strings.HasPrefix(part, "-") {
				return setupCommand{}, true, errSetupUsage
			}
			if cmd.email != "" {
				return setupCommand{}, true, errSetupUsage
			}
			email, err := oauth.NormalizeEmail(part)
			if err != nil {
				return setupCommand{}, true, err
			}
			cmd.email = email
		}
	}
	if cmd.email == "" {
		return setupCommand{}, true, errSetupUsage
	}
	return cmd, true, nil
}

func setupVerbRest(text string) (rest string, matched bool) {
	if text == setupVerb {
		return "", true
	}
	suffix, ok := strings.CutPrefix(text, setupVerb)
	if !ok || suffix == "" {
		return text, false
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	if !unicode.IsSpace(r) {
		return text, false
	}
	return strings.TrimSpace(suffix), true
}

// setupModeFlag is the CLI flag that selects an explicit setup mode, for copy.
func setupModeFlag(mode oauth.SetupMode) string {
	if mode == oauth.SetupModeRepoint {
		return setupFlagRepoint
	}
	return setupFlagRotate
}

// setupModeAction is the user-facing verb for an explicit setup mode, for copy.
func setupModeAction(mode oauth.SetupMode) string {
	switch mode {
	case oauth.SetupModeRotate:
		return "key rotation"
	case oauth.SetupModeRepoint:
		return "key repoint"
	case oauth.SetupModeReuse:
		return "setup"
	default:
		return "setup"
	}
}

// handleSetup mints a workspace-bound state token and replies with the
// /oauth/qurl/start URL. team_id + user_id come from the Slack form
// payload, which has already passed signing-secret verification — that
// chain is what binds workspace identity to the resulting state token
// (the alternative, taking team_id from an unsigned query param at
// /start, was the workspace-rebind primitive flagged in PR review).
//
// Surface: setup is a `/qurl` (user) verb, NOT on the admin `/qurl-admin`
// command. qURL is first-come-claims — on an unbound workspace the first
// user to complete setup becomes its owner — so the command must be
// reachable by any workspace member. Putting it on the admin-restricted
// `/qurl-admin` command would lock out the very first claimant, who is by
// definition not yet an admin of anything.
//
// It is still owner-gated against *rebind*: on fresh install (no
// workspace_mappings row) any workspace user may run /setup and becomes
// the workspace owner. First install is first-user-wins: if two members
// race an unbound workspace, BindWorkspace's consistent read picks the
// single winner (the loser gets the rebind-refused page). On subsequent
// runs only the owner is permitted; other workspace members (including
// admins added via `/qurl-admin admin add`) get an "owner-only" reply.
// Without this gate, any added admin could complete OAuth against their
// own Auth0 account and silently re-point the workspace's qURL credential —
// the OAuth callback's BindWorkspace pre-flight (see oauth.checkBindAllowed)
// also rejects that case as a defense in depth, but gating here means
// non-owners don't get a setup URL minted in their name at all (cleaner
// audit, no half-completed OAuth flows).
//
// AdminStore=nil (sandbox / no-DDB) permits first-time setup but rejects
// explicit rotation because rotation must prove the caller is the workspace
// owner before revoking a stored key. That is a separate short-circuit from
// the oauthSetup==nil check below, which is the branch that returns "qURL
// OAuth is not configured" (and which fires first, before AdminStore is
// consulted).
func (h *Handler) handleSetup(w http.ResponseWriter, values url.Values, setupCmd setupCommand) {
	if h.oauthSetup == nil {
		respondSlack(w, "qURL OAuth is not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	if teamID == "" || userID == "" {
		respondSlack(w, "Could not read your Slack workspace or user ID from the command payload.")
		return
	}
	if setupCmd.mode.Explicit() && h.cfg.AdminStore == nil {
		respondSlack(w, fmt.Sprintf("qURL workspace %s is not available on this Secure Access Agent deployment. Run `/qurl setup <email>` without `%s`, or contact the operator.", setupModeAction(setupCmd.mode), setupModeFlag(setupCmd.mode)))
		return
	}
	// Owner gate. AdminStore==nil only reaches here for first-time/reuse setup
	// (sandbox/no-DDB); explicit rotation/repoint was rejected above because it
	// cannot skip the owner check. Otherwise check whether the workspace has an owner
	// and whether it's the invoking user. CheckAdmin returns (isAdmin, ownerID,
	// err); we only consume ownerID here — the admin-set membership
	// is irrelevant for /setup specifically (added admins can't rerun
	// /setup, only the owner can). Times the read off h.baseCtx (not the
	// request ctx) so a Slack-side connection-close can't truncate the
	// gate read mid-flight; adminGateBudget is the only bound — same
	// posture as requireAdminSync.
	if h.cfg.AdminStore != nil {
		gateCtx, gateCancel := context.WithTimeout(h.baseCtx, adminGateBudget)
		defer gateCancel()
		_, ownerID, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
		if err != nil {
			slog.Error("/qurl setup: owner check failed", "error", err, "team_id", teamID, "caller_user_id", userID)
			respondSlack(w, ":warning: could not verify who connected qURL to this workspace (upstream error; see logs). Try again in a moment.")
			return
		}
		// ownerID=="" → workspace not yet bound → fresh install, allow.
		// (CheckAdmin reads eventually-consistent, and BindWorkspace
		// validates OwnerID != "" before PutItem, so an empty ownerID
		// almost always means "no row yet" rather than a half-written
		// one.) The one exception is a manually-edited row left with a
		// blank owner_id: it also reads as "" here and slips past this
		// gate, but BindWorkspace's consistent check refuses it (the
		// caller lands on the rebind-refused page after the OAuth round-
		// trip, and the empty-owner Warn there flags the bad row). That
		// requires DDB tampering, so it isn't worth a second read to
		// distinguish at the gate.
		// ownerID==userID → idempotent rerun by owner; the OAuth
		// callback reuses a healthy key and only replaces a missing
		// or revoked key, allow.
		// otherwise → non-owner rebind attempt, refuse here so we
		// don't even mint the state token / setup URL.
		//
		// This gate is best-effort: in the brief eventual-read window
		// after a fresh bind a fast second-mover could still see "" and
		// get a setup URL, but BindWorkspace's consistent owner check is
		// the structural backstop — that caller just lands on the
		// generic rebind-refused page instead of the friendly copy here.
		// The eventual read is deliberate, not an oversight: CheckAdmin
		// is the shared admin-gate read (same call the admin verbs make),
		// so the race only ever costs the loser a less-friendly error
		// page — never security, since the consistent backstop is
		// authoritative. Upgrading just this caller to a consistent read
		// would spend 2x RCU on every /setup to improve one racer's copy.
		if ownerID != "" && ownerID != userID {
			// Shape-guard the stored owner_id before interpolating it
			// into a `<@%s>` mention. BindWorkspace writes owner_id
			// from the OAuth callback (a different code path than the
			// parser), and a pre-pivot row holds an Auth0 sub, not a
			// Slack ID. Mirrors the looksLikeSlackUserID guard in
			// handleAdminList so a malformed value can't break out of
			// the mention surface.
			if looksLikeSlackUserID(ownerID) {
				slog.Warn("/qurl setup: rebind refused at slash-command gate — caller is not the workspace owner", "team_id", teamID, "caller_user_id", userID, "owner_user_id", ownerID)
				respondSlack(w, fmt.Sprintf("`/qurl setup <email>` can only be re-run by the person who first connected qURL to this workspace (<@%s>). This stops anyone else from re-pointing it at a different qURL account, so ask them to re-run it. For admin tasks that don't need re-connecting, use the `/qurl-admin` commands.", ownerID))
				return
			}
			// Shape-bad owner_id → a pre-pivot Auth0 sub left behind by
			// the #510 owner-model migration. No Slack user can ever
			// match it, so this workspace is locked for everyone unless
			// we let setup recover it. DON'T dead-end here — fall through
			// to mint the setup URL; BindWorkspace self-heals on the
			// callback by reclaiming the orphaned row for this caller
			// (first-come-claims, the same posture as an unbound
			// workspace). Log loudly so the legacy reclaim is grep-able.
			if setupCmd.mode.Explicit() {
				flag := setupModeFlag(setupCmd.mode)
				slog.Warn("/qurl setup: explicit mode refused for shape-bad legacy owner_id — require plain setup reclaim first", "team_id", teamID, "caller_user_id", userID, "mode", string(setupCmd.mode), "legacy_owner_prefix", slackdata.LegacyOwnerPrefix(ownerID), "owner_id_len", len(ownerID))
				respondSlack(w, fmt.Sprintf("`/qurl setup <email> %s` cannot run until this legacy workspace ownership record is reclaimed. Run `/qurl setup <email>` without `%s` first, then the recorded workspace owner can re-run it.", flag, flag))
				return
			}
			slog.Warn("/qurl setup: stored owner_id is shape-bad (likely a pre-pivot Auth0 sub) — allowing setup to reclaim the legacy row", "team_id", teamID, "caller_user_id", userID, "legacy_owner_prefix", slackdata.LegacyOwnerPrefix(ownerID), "owner_id_len", len(ownerID))
		}
	}
	state, err := oauth.MintStateWithEmailMode(h.oauthSetup.StateSecret, teamID, userID, setupCmd.email, setupCmd.mode, h.now())
	if err != nil {
		slog.Error("/qurl setup: MintStateWithEmailMode failed", "error", err)
		respondSlack(w, "Could not generate setup link. Please try again or contact support.")
		return
	}
	setupURL := h.oauthSetup.SetupURL(state)
	action := setupModeAction(setupCmd.mode)
	respondSlack(w, "Continue "+action+" for `"+echoText(setupCmd.email)+"`: <"+setupURL+"|Continue "+action+">\n\nAuth0 will ask you to sign in with that email after you continue. This link is valid for 5 minutes and only works for you.")
}

func (h *Handler) handleUninstall(w http.ResponseWriter, values url.Values) {
	teamID, userID, ok := h.requireUninstallAvailableAndAuthorized(w, values)
	if !ok {
		return
	}
	h.deleteWorkspaceAPIKey(w, teamID, userID)
}

// requireUninstallAvailableAndAuthorized is the single precondition path for
// both bare uninstall and uninstall argument variants.
func (h *Handler) requireUninstallAvailableAndAuthorized(w http.ResponseWriter, values url.Values) (teamID, userID string, ok bool) {
	teamID = strings.TrimSpace(values.Get(fieldTeamID))
	if teamID == "" {
		respondSlack(w, "Could not read your Slack workspace ID from the command payload.")
		return "", "", false
	}
	if h.cfg.AuthProvider == nil {
		respondUninstallUnavailable(w, uninstallUnavailableCredentialStorage)
		return "", "", false
	}
	userID = strings.TrimSpace(values.Get(fieldUserID))
	// Providers that cannot delete are structurally non-mutating. Return the
	// unsupported reply before any owner/user checks or delete context setup.
	if !h.cfg.AuthProvider.SupportsDeleteAPIKey() {
		respondUninstallUnsupported(w)
		return "", "", false
	}
	// Any provider that can delete must have AdminStore wired so this
	// destructive command is owner/admin-gated.
	if h.cfg.AdminStore == nil {
		slog.Error("/qurl uninstall: owner gate unavailable for mutable auth provider", "team_id", teamID, "caller_user_id", userID)
		respondUninstallUnavailable(w, uninstallUnavailableOwnerVerification)
		return "", "", false
	}
	if !h.requireUninstallAdminOrOwner(w, teamID, userID) {
		return "", "", false
	}
	return teamID, userID, true
}

// workspaceKeyRevoker is the optional capability a mutable AuthProvider
// implements to expose the stored qURL key_id so /qurl uninstall can revoke the
// upstream key before removing local credentials. *auth.DDBProvider implements
// it; providers that cannot (e.g. auth.EnvProvider) fall back to the local-only
// disconnect path. It is kept off the base auth.Provider interface — and
// discovered by type assertion — because non-Slack consumers (cli) have no
// per-workspace key_id, and unlike DeleteAPIKey there is no Supports() gate to
// piggyback on: the assertion itself is the capability check. The same
// strongly-read key_id powers owner-initiated rotation via oauth.WorkspaceStore,
// the same consumer-side narrow-interface pattern.
type workspaceKeyRevoker interface {
	APIKeyID(ctx context.Context, workspaceID string) (keyID string, err error)
}

// Ensure the production provider keeps satisfying the capability so a refactor
// that drops APIKeyID surfaces here rather than silently downgrading every
// uninstall to local-only.
var _ workspaceKeyRevoker = (*auth.DDBProvider)(nil)

func (h *Handler) deleteWorkspaceAPIKey(w http.ResponseWriter, teamID, userID string) {
	// Reuse the sync admin-verb budget (1.2s): after the owner/admin gate, the
	// optional upstream revoke plus the DeleteAPIKey write stay inside Slack's 3s
	// ack window. The revoke is best-effort within this ctx — the qURL client may
	// retry a flapping upstream (WithRetry), but the ctx bound makes a retry storm
	// abort (key_id preserved) rather than miss the ack.
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()

	// Shown only on the revoked=true paths (204/404), which are unreachable for a
	// self-revoke (see classifyUninstallRevokeError) — defensive for #806. The
	// "(or was already revoked upstream)" hedge covers the 404 case it would surface.
	const revokedReply = "qURL has been disconnected from this workspace's Slack commands, and this workspace's qURL API key has been revoked (or was already revoked upstream).\n\nThe recorded workspace owner can run `/qurl setup <email>` to reconnect it."

	// revoked reports whether the upstream key was revoked; a non-nil error means
	// the key may still be live, so abort before local removal to preserve the
	// stored key_id for a retry rather than orphaning it.
	revoked, err := h.revokeWorkspaceUpstreamKey(ctx, teamID, userID)
	if err != nil {
		// Covers all abort arms — a failed revoke, but also the key_id read and
		// client-build (KMS) failures where no revoke was even attempted — so the
		// copy says "disconnect", not "revoke".
		respondSlack(w, ":warning: Couldn't disconnect qURL right now. Nothing was disconnected — try again in a moment, and contact your qURL operator if it keeps failing.")
		return
	}

	if err := h.cfg.AuthProvider.DeleteAPIKey(ctx, teamID); err != nil {
		switch {
		case errors.Is(err, auth.ErrWorkspaceNotConfigured):
			if revoked {
				// The row's qURL columns were already cleared (concurrent uninstall
				// / partial row), but this call did revoke the upstream key — report
				// success, not the contradictory "isn't currently connected".
				slog.Info("/qurl uninstall: upstream key revoked; local row already cleared", "team_id", teamID, "caller_user_id", userID)
				respondSlack(w, revokedReply)
				return
			}
			respondSlack(w, "qURL isn't currently connected to this workspace. The recorded workspace owner can run `/qurl setup <email>` to connect it; contact your qURL operator if the owner is unavailable.")
			return
		case errors.Is(err, auth.ErrWorkspaceAPIKeyDeleteUnsupported):
			respondUninstallUnsupported(w)
			return
		default:
			slog.Error("/qurl uninstall: DeleteAPIKey failed", "error", err, "team_id", teamID, "caller_user_id", userID)
			respondSlack(w, ":warning: could not disconnect qURL from this workspace. Try again in a moment.")
			return
		}
	}
	slog.Info("/qurl uninstall: disconnected workspace Slack commands", "team_id", teamID, "caller_user_id", userID, "upstream_revoked", revoked)
	if revoked {
		respondSlack(w, revokedReply)
		return
	}
	respondSlack(w, "qURL has been disconnected from this workspace's Slack commands.\n\nThis does not revoke the qURL API key outside Slack; contact the operator if you're disconnecting because the key may be exposed.\n\nThe recorded workspace owner can run `/qurl setup <email>` to reconnect it.")
}

// revokeWorkspaceUpstreamKey best-effort revokes the workspace's upstream qURL
// API key before local credential removal, using the strongly-read stored key_id.
// It returns revoked=true only when the key was already gone upstream (404);
// (false, nil) for a local-only disconnect (no key_id, a legacy row, or the
// expected 403/401 self-revoke refusal); and a non-nil error when a
// transient/unexpected failure left the key possibly live, so the caller aborts
// before DeleteAPIKey and the stored key_id survives for a retry rather than
// orphaning it. classifyUninstallRevokeError is the authoritative record of the
// confirmed self-revoke contract (#805) these outcomes follow.
func (h *Handler) revokeWorkspaceUpstreamKey(ctx context.Context, teamID, userID string) (revoked bool, err error) {
	revoker, ok := h.cfg.AuthProvider.(workspaceKeyRevoker)
	if !ok {
		return false, nil
	}
	keyID, err := revoker.APIKeyID(ctx, teamID)
	switch {
	case errors.Is(err, auth.ErrWorkspaceNotConfigured):
		// No readable key to revoke. DeleteAPIKey owns the not-connected vs
		// partial-row-cleanup messaging, so fall through to it.
		return false, nil
	case err != nil:
		slog.Error("/qurl uninstall: could not read stored qURL key id before revoke", "error", err, "team_id", teamID, "caller_user_id", userID)
		return false, err
	}
	if keyID == "" {
		slog.Warn("/qurl uninstall: legacy workspace row has no qURL key id — local-only disconnect", "team_id", teamID, "caller_user_id", userID)
		return false, nil
	}

	// This is deliberately a second workspace read: APIKeyID above returns the
	// key_id, while authenticatedClient needs the KMS-decrypted plaintext key. The
	// rotation path's combined APIKeyIdentity read returns key_id + account, not
	// the plaintext, so it can't be reused here. The second read is ttlcache-backed
	// and both stay inside the uninstall sync budget.
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
			// Key vanished between the key_id read and here; DeleteAPIKey is the
			// not-connected authority.
			return false, nil
		}
		slog.Error("/qurl uninstall: could not build client to revoke upstream key", "error", err, "team_id", teamID, "caller_user_id", userID)
		return false, err
	}
	// For a live key this DELETE always 403s → local-only degrade (by design, see
	// classifyUninstallRevokeError); the call is kept for the #806 owner-auth path.
	if err := c.RevokeAPIKey(ctx, keyID); err != nil {
		return classifyUninstallRevokeError(err, teamID, userID, keyID)
	}
	slog.Info("/qurl uninstall: revoked upstream qURL API key", "team_id", teamID, "caller_user_id", userID, "key_id", keyID)
	return true, nil
}

// classifyUninstallRevokeError maps a RevokeAPIKey failure to (revoked, err),
// per the confirmed qurl-service contract for DELETE /v1/api-keys/{key_id}.
//
// Confirmed contract (#805): uninstall has no live OAuth flow, so it
// authenticates this DELETE with the workspace's own API key, and qurl-service
// categorically forbids an API key from managing API keys — the DELETE returns
// 403 ("API keys cannot manage API keys. Use JWT authentication.") for ANY
// target key_id, including its own, structurally and before any existence check.
// So a live workspace key ALWAYS gets 403 here: revoking the upstream key from
// /qurl uninstall is a local-only disconnect by design, not a deployment quirk.
// (Rotation can revoke because oauth/callback runs under the owner's freshly
// authenticated JWT; uninstall has only the workspace API key.)
//
// Status handling under that contract:
//   - 403: self-revoke forbidden — the universal case for a live key. Degrade to
//     a local-only disconnect (false, nil); the upstream key stays live, so the
//     caller keeps the "not revoked outside Slack" caveat and a security-motivated
//     uninstall still needs a manual upstream revoke (see operating.md).
//   - 401: the workspace key is already revoked/expired upstream (e.g. a prior
//     rotation) — qurl-service's auth middleware rejects the credential with 401
//     before the existence check. The key is already dead, so degrade too; the
//     caveat then over-warns, the safe direction. The logged `status` field
//     distinguishes 401 from 403 for operators.
//   - 404: treat as revoked (true, nil) — but UNREACHABLE for a self-revoke. The
//     credential IS the target key, so a present key 403s and an absent/dead key
//     401s, both before the existence check; there is no "valid credential +
//     missing target" path. This arm — like the 204 success path in
//     revokeWorkspaceUpstreamKey — is defensive scaffolding for a future
//     owner-authenticated revoke (#806), where the credential and target differ
//     and a 404 (or a real 204) can occur.
//   - everything else (other 4xx, 5xx, transport, timeout): possibly-still-live —
//     return the error so the caller aborts and the stored key_id survives.
//
// Net for a self-revoke: only 403/401 (→ degrade) and 5xx/transport (→ abort)
// occur in prod, so `upstream_revoked` is effectively always false and the
// revoked=true arms never fire until the #806 owner-auth path lands.
//
// 401/403 are structural, never transient FOR A LIVE KEY: 403 is a deterministic
// gate, and qurl-service's auth middleware returns 401 only for a genuinely
// revoked/expired/unknown credential — a still-live key passes auth and reaches
// the 403 gate, while an infra blip (e.g. a datastore lookup error) surfaces as
// 5xx, not 401. So a 401 here always means the key is already dead, and degrading
// 401/403 to local-only cannot strand a still-live key behind a transient auth
// blip (a 5xx lands on the abort arm instead). That upstream contract is pinned
// by qurl-service's middleware tests: TestAPIKeyAuth_ValidKey (live → passes →
// 403), TestAPIKeyAuth_RevokedKey / TestAPIKeyAuth_ExpiredKey (dead → 401), and
// TestAPIKeyAuth_RepoError_Returns500 (infra → 500, not 401).
//
// Flipping 401/403 to abort (tell the admin to retry/escalate instead of a silent
// local-only disconnect) is deliberately sequenced behind the force/local-only
// escape hatch (#806); until that lands the degrade is the safe interim — without
// it, a deployment that refuses the revoke would leave admins unable to
// disconnect at all.
func classifyUninstallRevokeError(revokeErr error, teamID, userID, keyID string) (revoked bool, err error) {
	var apiErr *client.APIError
	if errors.As(revokeErr, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusNotFound:
			slog.Info("/qurl uninstall: upstream qURL key already absent — treating as revoked", "team_id", teamID, "caller_user_id", userID, "key_id", keyID)
			return true, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// Expected, by-design path: qurl-service forbids API-key self-revoke
			// (403) and rejects an already-revoked key (401). Warn, not Error —
			// the `status` field distinguishes the still-live 403 from the
			// already-dead 401 for operators (see operating.md).
			slog.Warn("/qurl uninstall: upstream qURL key not revoked — local-only disconnect", "status", apiErr.StatusCode, "team_id", teamID, "caller_user_id", userID, "key_id", keyID)
			return false, nil
		}
	}
	slog.Error("/qurl uninstall: upstream qURL key revoke failed — aborting to preserve key id", "error", revokeErr, "team_id", teamID, "caller_user_id", userID, "key_id", keyID)
	return false, revokeErr
}

func respondUninstallUnsupported(w http.ResponseWriter) {
	respondSlack(w, "`/qurl uninstall` isn't supported on this Secure Access Agent deployment. Contact the operator.")
}

type uninstallUnavailableReason int

const (
	uninstallUnavailableCredentialStorage uninstallUnavailableReason = iota
	uninstallUnavailableOwnerVerification
)

// respondUninstallUnavailable is the shared unavailable-reason-to-operator-copy
// map for bare uninstall and uninstall argument variants.
func respondUninstallUnavailable(w http.ResponseWriter, reason uninstallUnavailableReason) {
	switch reason {
	case uninstallUnavailableCredentialStorage:
		respondSlack(w, "qURL credential storage is not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	case uninstallUnavailableOwnerVerification:
		respondSlack(w, "qURL owner verification is not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	default:
		slog.Error("/qurl uninstall: unknown unavailable reason", "reason", int(reason))
		respondSlack(w, "qURL uninstall is not available on this Secure Access Agent deployment. Contact the operator.")
		return
	}
}

func (h *Handler) canAdvertiseUninstall() bool {
	return h.cfg.AdminStore != nil && h.cfg.AuthProvider != nil && h.cfg.AuthProvider.SupportsDeleteAPIKey()
}

func (h *Handler) requireUninstallAdminOrOwner(w http.ResponseWriter, teamID, userID string) bool {
	if userID == "" {
		respondSlack(w, ":warning: missing user_id in slash command payload")
		return false
	}
	// CheckAdmin intentionally uses the same low-latency admin-store read as
	// other Slack admin verbs; uninstall can be reconnected by the recorded
	// owner/operator if a just-revoked admin races DynamoDB replication.
	ctx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, ownerID, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		slog.Error("/qurl uninstall: owner check failed", "error", err, "team_id", teamID, "caller_user_id", userID)
		respondSlack(w, ":warning: failed to verify who connected qURL to this workspace (upstream error; see logs). Try again in a moment.")
		return false
	}
	// Issue #268 asks for workspace-admin self-service offboarding. Setup stays
	// owner-only because it changes the key binding; uninstall only disconnects
	// the existing Slack command mapping. CheckAdmin returns true for both the
	// recorded workspace owner and explicit qURL workspace admins.
	if isAdmin {
		// Missing or legacy owner metadata should not strand an explicit admin
		// from this recoverable local disconnect; log it for operator cleanup.
		if ownerID == "" {
			slog.Warn("/qurl uninstall: admin allowed with missing owner_id", "team_id", teamID, "caller_user_id", userID)
		} else if !looksLikeSlackUserID(ownerID) {
			slog.Warn("/qurl uninstall: admin allowed with shape-bad owner_id", "team_id", teamID, "caller_user_id", userID, "owner_id_len", len(ownerID))
		}
		return true
	}
	slog.Warn("/qurl uninstall: non-admin denied", "team_id", teamID, "caller_user_id", userID, "owner_id_len", len(ownerID))
	respondSlack(w, "`/qurl uninstall` can only be run by a qURL workspace admin or by the person who connected qURL to this workspace.")
	return false
}

// authenticatedClient resolves an API key for the team and returns a configured client.
func (h *Handler) authenticatedClient(ctx context.Context, teamID string) (*client.Client, error) {
	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		return nil, err
	}
	return h.cfg.NewClient(apiKey), nil
}

func (h *Handler) handleEvent(w http.ResponseWriter, body []byte) {
	var env slackEventEnvelope
	switch err := json.Unmarshal(body, &env); {
	case err != nil:
		// Bad JSON shouldn't 4xx (Slack retries on non-2xx). Surface
		// the parse error at Debug so spec drift / corrupt payloads
		// are visible to operators without breaking the contract.
		slog.Debug("event JSON parse failed", "error", err, "body_length", len(body))
	case env.Type == "url_verification":
		respondJSON(w, http.StatusOK, map[string]string{"challenge": env.Challenge})
		return
	case env.Type == "event_callback":
		// Conversation mode. handleAgentEvent only schedules async work (or
		// no-ops when disabled/filtered); we always ack 200 below so Slack
		// never retries a delivery we accepted.
		h.handleAgentEvent(&env)
	}

	respondJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// userHelpMessage renders the `/qurl help` text — the user-facing verbs
// only. Verbs that depend on optional Config wiring are omitted when that
// wiring is nil — a workspace without PostDM won't see the dm:true
// variant. The verbs still dispatch if a user types them directly; the
// omission is just so help text doesn't advertise a path that will reply
// with ":warning: not configured". The admin verbs live on `/qurl-admin`
// (see [Handler.adminHelpMessage]); a pointer line routes admins there.
func (h *Handler) userHelpMessage(command string) string {
	// command is non-empty here — handleSlashCommand normalizes an empty
	// payload to commandUser before dispatch — so the ReplaceAll below
	// always has a non-empty base (an empty base would strip every `/qurl`).
	lines := []string{
		"*/qurl* — Create and manage qURLs from Slack",
		"",
		"*Commands:*",
	}
	// setup is a user verb (first-come-claims), so it leads the user
	// surface. The owner semantics only exist when AdminStore is wired; on
	// the sandbox/no-DDB path the owner gate in handleSetup is skipped and
	// the OAuth callback still owns key reuse/replacement. Append the owner
	// parenthetical only there so the help text matches the deployment's
	// actual behavior.
	setupLine := "• `/qurl setup <email>` — Connect qURL to your Slack workspace"
	if h.cfg.AdminStore != nil {
		setupLine += " (whoever first runs it is the only one who can re-run it — this keeps the workspace's qURL account from being switched to someone else)"
	}
	lines = append(lines, setupLine)
	if h.canAdvertiseUninstall() {
		lines = append(lines, "• `/qurl uninstall` — Disconnect qURL from this Slack workspace")
	}
	if h.cfg.AdminStore != nil {
		// Glossary so the `$slug` / `$alias` tokens in the verbs and in
		// `/qurl list` aren't unexplained. Only shown when AdminStore is
		// wired — that's the only deploy where aliases exist.
		//
		// get resolves its $slug/$alias token through resolveTokenForGet,
		// which fails closed (":warning: not configured") when AdminStore is
		// nil — the URL form that once let get work without DDB is gone
		// post-tunnels-only. Gate the get verbs on AdminStore so help never
		// advertises a verb whose only reply would be the not-configured
		// error (same rule as `/qurl aliases` below).
		lines = append(lines,
			"• `/qurl setup <email> --rotate` — Replace the workspace qURL key on the same qURL account",
			"• `/qurl setup <email> --repoint` — Move the workspace to a different qURL account (cross-account moves route to an operator)",
			"_`$id` identifies a resource. A `$alias` is an alternate name for a resource in a channel — several aliases can point to one ID. Use either with `/qurl get`._",
			"",
			"• `/qurl get <$id|$alias>` — Create a qURL for a resource `$id` or a `$alias` configured in this channel",
		)
		if h.cfg.PostDM != nil {
			lines = append(lines, "• `/qurl get <$id|$alias> dm:true` — DM the link to you instead of posting it in-channel")
		}
		lines = append(lines,
			"• `/qurl get <$id|$alias> reason:\"…\"` — Create a qURL, recording a reason in the audit log",
		)
	}
	lines = append(lines,
		"• `/qurl list` — List the resources available to you",
	)
	if h.cfg.AdminStore != nil {
		// aliases reads channel_policies through the AdminStore (NOT the
		// aliasStore that set-alias/unset-alias write through), so it
		// gates on AdminStore to match processAliases's own nil-check —
		// otherwise help could advertise `/qurl aliases` on a deploy where
		// it replies ":warning: not configured".
		lines = append(lines,
			"• `/qurl aliases` — List this channel's aliases and the resource each one points to",
		)
	}
	if h.cfg.PostFeedback != nil {
		// feedback needs no AdminStore/setup — only the PostFeedback seam —
		// so it gates on that alone and shows even on no-DDB deploys.
		lines = append(lines,
			"• `/qurl feedback` — Send a bug report or feature request to the qURL team",
		)
	}
	lines = append(lines,
		"• `/qurl help` — Show this help message",
		"",
		"Admins: run `/qurl-admin help` for resource setup, alias, and admin commands.",
	)
	// The lines are authored with the prod command names (`/qurl`,
	// `/qurl-admin`). Rewrite the `/qurl` prefix to the invoked user
	// command so a non-prod env renders its own names — and because every
	// admin literal here is `/qurl-admin` == `/qurl` + adminCommandSuffix,
	// the same replace also fixes the admin pointer line
	// (`/qurl-sandbox` → `/qurl-sandbox-admin help`). command is the user
	// command on this surface, so the replacement is a no-op in prod.
	//
	// MAINTAINER INVARIANT: ReplaceAll is blind, so every `/qurl` substring
	// in `lines` must be a command literal — keep non-command prose (URLs
	// like `qurl.link`, `/qurl-foo` examples) free of the lowercase `/qurl`
	// token, or a non-prod env rewrites them too.
	// TestHelpMessagesContainOnlyCommandTokens guards this: a stray
	// non-command slash token fails there.
	return strings.ReplaceAll(strings.Join(lines, "\n"), commandUser, command)
}

// adminHelpMessage renders the `/qurl-admin help` text — the admin-gated
// verbs only (setup is a user verb and lives in [Handler.userHelpMessage]).
// The conditional gating mirrors what each verb actually does at runtime —
// a verb whose only reply would be ":warning: not configured" (aliasStore,
// AdminStore, OpenView all nil on sandbox deploys) is omitted so help never
// advertises a path the user can't take. These commands are admin-only,
// enforced in code: every admin verb runs requireAdminSync against the
// qURL admin set (see handleSetAlias). The `/qurl-admin` registration
// should also be marked admin-only in the Slack app config, but that is a
// cosmetic picker hint — Slack does not gate slash-command invocation on
// workspace-admin role — not the enforcement boundary.
func (h *Handler) adminHelpMessage(command string) string {
	// command is non-empty here (normalized in handleSlashCommand); see
	// userHelpMessage for why the ReplaceAll below needs a non-empty base.
	// The verbs are grouped under bold section headers (Protect resources,
	// Aliases, Manage resources, Admins) rather than one flat bullet list —
	// the admin surface grew long enough that a flat list was hard to scan. Each
	// section is gated on the same wiring its verbs need at runtime, so an
	// unwired deploy never renders an empty header; in practice aliasStore and
	// AdminStore are wired in lockstep (both from the QURL_*_TABLE env vars; see
	// cmd/main.go), so the sections appear and disappear together.
	lines := []string{
		"*/qurl-admin* — Admin commands for qURL in Slack",
	}
	appendSectionHeader := func(title string) {
		lines = append(lines, "", title)
	}
	// Protect resources: stand up new access in this channel (a connector tunnel
	// or an existing URL resource). Gates on aliasStore + AdminStore, the same
	// pair the install/protect verbs need; the guided-vs-typed split nests under
	// OpenView, the condition the guided modals themselves require.
	if h.aliasStore != nil && h.cfg.AdminStore != nil {
		appendSectionHeader("*Protect resources*")
		if h.cfg.OpenView != nil {
			lines = append(lines,
				"• `/qurl-admin protect` — Guided chooser: protect a connector service or an existing URL resource (recommended)",
				"• `/qurl-admin protect-connector` — Guided connector setup (Docker, Docker Compose, ECS/Fargate, Kubernetes)",
				"• `/qurl-admin protect-connector <id> [env:...] [port:8080] [alias:$alias]` — Typed connector setup; creates a bootstrap key and binds `$<id>` in this channel",
				"• `/qurl-admin protect-url` — Guided URL picker; choose an existing URL resource and channel alias",
				"• `/qurl-admin protect-url $<alias> [as:$channel-alias]` — Typed: protect an existing URL resource in this channel",
				"• `/qurl-admin protect-url url:<target-url> as:$channel-alias` — Typed: protect an existing no-alias URL resource in this channel",
				"• Typed connector options: `env:docker|docker-compose|ecs-fargate|kubernetes`; Docker accepts `container:<name>` or `web_container:<name>`; Compose accepts `service:<name>`; `env:compose` also works",
			)
		} else {
			lines = append(lines,
				"• `/qurl-admin protect-connector <id> [env:...] [port:8080] [alias:$alias]` — Create a sidecar bootstrap key and bind `$<id>` in this channel",
				"• `/qurl-admin protect-url $<alias> [as:$channel-alias]` — Protect an existing URL resource in this channel",
				"• `/qurl-admin protect-url url:<target-url> as:$channel-alias` — Protect an existing no-alias URL resource in this channel",
				"  Guided setup (bare `/qurl-admin protect-connector` / `protect-url`) is not enabled in this deployment; use the typed forms above.",
			)
		}
	}
	if h.aliasStore != nil {
		// Aliases: alternate names that resolve to a tunnel within a channel.
		// set-alias/unset-alias reply ":warning: not configured" on a sandbox
		// deploy without an aliasStore, so gate on it — help shouldn't advertise
		// verbs whose only reply tells the user they can't be used. User-facing
		// copy calls these "aliases" (not "shortcuts") even though the admin
		// verbs retain their historical set-alias/unset-alias names.
		//
		// Gates on aliasStore (the store set-alias/unset-alias WRITE through).
		// At runtime these verbs ALSO need AdminStore for the in-code
		// requireAdminSync gate (see handleSetAlias), but aliasStore and
		// AdminStore are wired in lockstep (both from the same QURL_*_TABLE env
		// vars; see cmd/main.go), so gating here on aliasStore is equivalent to
		// gating on both. `/qurl aliases` gates on AdminStore because it READS
		// channel_policies through it.
		appendSectionHeader("*Aliases*")
		lines = append(lines,
			"• `/qurl-admin set-alias $<alias> $<id>` — Point an alias at a qURL Connector ID in this channel",
			"• `/qurl-admin unset-alias $<alias>` — Remove an alias from this channel",
		)
	}
	if h.cfg.AdminStore != nil {
		// Every verb below gates on the in-code requireAdminSync (CheckAdmin
		// against AdminStore), so they're listed only when AdminStore is wired —
		// the same condition the verbs use at runtime. On sandbox deploys without
		// the three QURL_*_TABLE env vars (see cmd/main.go), AdminStore is nil and
		// these verbs render "Admin features are not configured", so gating the
		// help lines on the same condition keeps the listing consistent with what
		// the verbs actually do.
		//
		// Two sections under the one AdminStore gate:
		//   Manage resources — name a resource (set-/unset-display-name set the
		//     friendly Display Name shown in `/qurl list`) or retire it (revoke
		//     is resource-scoped via `$<id>`).
		//   Admins — who's allowed to run these commands. Flat membership
		//     verbs (no `admin` sub-word); `admins` is the plural-noun roster,
		//     so it doesn't collide with `/qurl list`.
		appendSectionHeader("*Manage resources*")
		lines = append(lines,
			"• `/qurl-admin set-display-name $<id> <display name>` — Set a qURL Connector's friendly Display Name shown in `/qurl list`",
			"• `/qurl-admin unset-display-name $<id>` — Reset a qURL Connector's Display Name to the default",
			"• `/qurl-admin revoke $<id>` — Revoke a protected resource and all its qURLs",
		)
		appendSectionHeader("*Admins*")
		lines = append(lines,
			"• `/qurl-admin add @user` — Promote a Slack user to admin",
			"• `/qurl-admin remove @user` — Demote a Slack user from admin",
			"• `/qurl-admin admins` — List who connected qURL (the owner) and the current admins",
		)
		appendSectionHeader("*Conversation mode*")
		lines = append(lines,
			"• `/qurl-admin agent on` — Let members @mention or DM the qURL Secure Access Agent in this workspace",
			"• `/qurl-admin agent off` — Turn conversation mode off for this workspace",
			"• `/qurl-admin agent` — Show whether conversation mode is on for this workspace",
		)
	}
	// Always-present anchor: the sections above are all gated on sandbox wiring,
	// so without this line a no-store deploy would render just the header with no
	// verbs. Mirrors the `/qurl help` line on the user surface.
	lines = append(lines, "", "• `/qurl-admin help` — Show this help message")
	// Authored with the prod admin command name; rewrite to the invoked
	// admin command so a non-prod env renders its own (`/qurl-sandbox-admin`
	// …). Every admin literal here is the full `/qurl-admin`, so a single
	// replace covers them all; command is the admin command on this
	// surface, so the replacement is a no-op in prod.
	return strings.ReplaceAll(strings.Join(lines, "\n"), commandAdmin, command)
}

// respondMethodNotAllowed writes 405 with an RFC 7231 §6.5.5 Allow header.
// The header is the discriminator that lets ops separate "wrong method"
// from "missing path" (404) and "auth-gated" (401).
func respondMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// respondPayloadTooLarge writes 413 for both the Content-Length pre-check
// and the MaxBytesReader-during-read paths. Centralizing keeps the wire
// envelope identical so operator dashboards bucket them together.
func respondPayloadTooLarge(w http.ResponseWriter) {
	respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		// Marshaling a map[string]string / map[string]any can't fail in
		// practice; log and fall back to a fixed JSON envelope so the
		// Content-Type header doesn't disagree with the body.
		slog.Error("response marshal failed", "error", err)
		b = []byte(internalErrorEnvelope)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		slog.Warn("response write failed", "error", err)
	}
}

// Slack slash-command response keys + the ephemeral response-type value.
// Centralized so respondSlack and the parallel writer in postResponse
// can't drift, and so the goconst/keyword consistency stays linter-clean.
const (
	respFieldResponseType    = "response_type"
	respFieldText            = "text"
	respFieldReplaceOriginal = "replace_original"
	respTypeEphemeral        = "ephemeral"
	// respFieldResponseAction / respFieldView are the view_submission reply
	// keys (response_action: "errors"|"update" + the replacement view).
	respFieldResponseAction = "response_action"
	respFieldView           = "view"
	// respActionUpdate is the response_action value that swaps the current
	// modal for a replacement view (the modal error responders use it).
	respActionUpdate = "update"
)

func respondSlack(w http.ResponseWriter, text string) {
	respondJSON(w, http.StatusOK, map[string]string{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         text,
	})
}
