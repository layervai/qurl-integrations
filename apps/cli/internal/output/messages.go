package output

// Fixed customer-facing strings for error rendering, defined once so the
// jargon gate can assert over each. Hints follow the §17.1 anatomy: what
// happened, then the one next step most likely to fix it.
const (
	errorPrefix = "Error:"

	// msgLinksUnavailable renders the typed "temporary access links are not
	// being served here" condition (HTTP 503 on resolve) as a service
	// posture, not a user mistake.
	msgLinksUnavailable = "Temporary access links aren't available from this qURL endpoint right now. The resource may exist, but this environment isn't serving links for it yet. Try again later, or check that you're using the endpoint this CRID was published to."

	// msgNoCredential renders the missing-API-key condition.
	msgNoCredential = "No qURL API key is configured."

	hintNoCredential  = "Hint: set QURL_API_KEY, or run `qurl login` to store a key on this machine."
	hintUnauthorized  = "Hint: the service rejected your API key. Check QURL_API_KEY, or ask your qURL administrator for a new key."
	hintNotFound      = "Hint: the CRID may be mistyped, expired, or no longer published. Ask whoever shared it for a current one."
	hintQuotaExceeded = "Hint: you've reached your plan's limit. See https://layerv.ai/pricing to raise it."
	hintRetryAfter    = "Retry after %ds."

	// hintRevoked is owner-truthful: the platform tells a resource's owner
	// that their own resource was deleted rather than hiding it behind the
	// ambiguous not-found. Everyone else gets the ambiguous 404.
	hintRevoked = "Hint: this resource was deleted. Deleted resources stop resolving; publish the target again to get a new CRID."

	// hintRetired covers the permanently-closed lifecycle state.
	hintRetired = "Hint: this resource was permanently retired and will never resolve again. Publish the target again to get a new CRID."

	// hintScope covers a key that authenticates but cannot resolve.
	hintScope = "Hint: your API key isn't allowed to request access links. Ask your qURL administrator for a key with resolve access."

	// hintFrozen is the account-standing message for 403 account_frozen: the
	// key is fine, the account is paused — a materially different situation
	// from a permissions problem, so it must not hide behind a generic
	// forbidden hint.
	hintFrozen = "Hint: your qURL account is frozen, so requests are paused even though your API key is valid. Contact your qURL administrator or LayerV support to restore the account."

	// hintExpired tells an expired key apart from a rejected one: the remedy
	// is a new key, not a retyped one.
	hintExpired = "Hint: this API key has expired. Create a new key in the qURL console and run `qurl login` again."

	// hintKeyInvalid covers the platform's explicit not-a-key answer. Unlike
	// the generic 401 hint it does not steer to QURL_API_KEY: the key in
	// hand — typed at login or stored — is the thing the service refused.
	hintKeyInvalid = "Hint: the qURL service doesn't recognize this API key. Re-copy it from the qURL console, then run `qurl login` again (or update QURL_API_KEY if that's where it lives)."

	// Storage backend labels used in login/logout confirmations.
	labelKeyring        = "OS keyring"
	labelCredentialFile = "credential file"

	// labelCRID prefixes the copyable identity line every document that has
	// a CRID ends with, so publish and the Connector serve note cannot drift
	// into two spellings of the same label.
	labelCRID = "CRID:"

	// msgLoggedInAs opens the login confirmation; %s is the account.
	msgLoggedInAs = "Logged in as %s."

	// msgLoggedOut confirms removal; %s lists the storage that held the key.
	msgLoggedOut = "Logged out. Removed your qURL API key from the %s."

	// msgNothingStored is logout's idempotent no-op note (still exit 0).
	msgNothingStored = "No qURL API key is stored on this machine; nothing to remove."

	// msgSavedTo confirms a completed download: destination, then size.
	msgSavedTo = "Saved to %s (%d bytes)."

	// msgAlreadyPublished is the stderr status note for publishing a URL
	// that already has an active resource, in the modes whose stdout
	// documents must not change shape (--quiet and JSON); full text mode
	// says it in the document itself via msgPublishFoundExisting.
	msgAlreadyPublished = "This target was already published; showing the existing resource."

	// msgPublishFoundExisting is the text-document note for that same case:
	// what happened, then the one next step.
	msgPublishFoundExisting = "This URL already has an active resource, so its existing CRID is shown. Delete it first to publish the URL as a new resource."

	// msgConnectorServing announces the serve loop: the Connector ID (as the
	// platform records it), then the local app being served. It lives here
	// rather than in the cmd package because it is no longer a bare note —
	// Printer.ConnectorServing renders it as a document whose styling is
	// this package's private business.
	msgConnectorServing = "Starting Connector %q for your local app at %s. Press Ctrl-C to stop."

	// msgConnectorReachIt is that document's detail line: what the CRID
	// printed beneath it is for. It only renders when the platform actually
	// returned a CRID, so it can never point at a value that isn't there.
	msgConnectorReachIt = "Anyone authorized can reach it with `qurl get <CRID>`."

	// Connector lifecycle renderings. Headlines say what happened in customer
	// language; the operator detail (which stays technical) is appended by
	// RenderError as an indented detail block where it adds facts the
	// headline cannot carry, and each hint is the one next step.

	// msgConnectorTokenRequired renders agent.ErrEnrollmentTokenRequired.
	msgConnectorTokenRequired = "This machine isn't enrolled as a qURL Connector yet, and no enrollment token is configured."

	hintConnectorTokenRequired = "Hint: set QURL_CONNECTOR_TOKEN (or point QURL_CONNECTOR_TOKEN_FILE at a file holding the token) and run the command again. Enrollment tokens are single-use — create one in the qURL console. There is deliberately no flag for it: command-line arguments leak into shell history."

	// msgConnectorIdentityConflict renders agent.ErrIdentityConflict.
	msgConnectorIdentityConflict = "The Connector identity this command was configured with doesn't match the identity already stored on this machine."

	hintConnectorIdentityConflict = "Hint: remove the LAYERV_AGENT_ID override to keep using the stored identity, or point --state-dir at a fresh directory to enroll this machine separately."

	// msgConnectorRefreshApproval renders agent.ErrRefreshApprovalRequired.
	msgConnectorRefreshApproval = "This Connector needs its qURL platform assignment refreshed, and that refresh waits for your explicit approval."

	hintConnectorRefreshApproval = "Hint: review why it stopped (see your previous run's output), then run `qurl connector run` once with --refresh-mode auto to approve the refresh, and return to manual afterwards. Automatic restarts are deliberately not treated as approval."

	// msgConnectorRefreshModeInvalid renders agent.ErrRefreshModeInvalid —
	// the env-sourced refresh mode carries a value that is not a mode.
	msgConnectorRefreshModeInvalid = "The Connector's refresh-mode setting isn't one of the recognized values."

	// hintConnectorRefreshModeInvalid names the variable and the vocabulary.
	hintConnectorRefreshModeInvalid = "Hint: set LAYERV_AGENT_REGISTRATION_REFRESH_MODE to manual, auto, or disabled (or pass --refresh-mode)."

	// msgConnectorRefreshDisabled renders agent.ErrRefreshDisabled.
	msgConnectorRefreshDisabled = "This Connector needs its qURL platform assignment refreshed, but refreshes are disabled by its configuration."

	hintConnectorRefreshDisabled = "Hint: run once with --refresh-mode auto (or set LAYERV_AGENT_REGISTRATION_REFRESH_MODE to manual or auto), or clear the Connector's state directory and enroll this machine again with a new token."

	// msgConnectorRefreshExhausted renders agent.ErrRefreshAlreadyAttempted.
	msgConnectorRefreshExhausted = "This Connector already used its one automatic assignment refresh for this outage and still can't connect."

	hintConnectorRefreshExhausted = "Hint: this usually means a network problem between this machine and the qURL platform. Check outbound connectivity and try again; if it keeps happening, contact LayerV support before clearing any Connector state."

	// msgConnectorRetryBudget renders the supervisor's retry-budget exit
	// (IsTooManyKnockFailures).
	msgConnectorRetryBudget = "The qURL platform kept refusing or not answering this Connector's connection attempts, so it stopped rather than retry forever."

	hintConnectorRetryBudget = "Hint: check this machine's outbound network access, then run `qurl connector run` again. If the problem persists, the next start will ask to refresh this Connector's platform assignment (--refresh-mode auto approves it once)."

	// msgConnectorPreviousSessionActive renders the supervisor's retry-budget
	// exit when the last cause was the tunnel server's duplicate-session
	// refusal — the one stale-session condition the platform states in words
	// rather than leaving as an unexplained transport failure. It is checked
	// before msgConnectorRetryBudget precisely because that generic rendering
	// ("kept refusing or not answering") would send the reader to look at
	// their network, which is the wrong place for this cause.
	msgConnectorPreviousSessionActive = "This Connector's previous session is still registered with the qURL platform, so this one couldn't take over and it stopped after retrying."

	// hintConnectorPreviousSessionActive gives the wait-then-retry step first,
	// then the one thing the operator can change next time: how the previous
	// Connector was stopped decides whether the platform is told to release
	// the session or has to time it out.
	hintConnectorPreviousSessionActive = "Hint: wait about a minute for the qURL platform to release the previous session, then run `qurl connector run` again. Stopping a Connector with Ctrl-C releases its session right away — one that was force-killed, or that lost its network connection before it stopped, stays registered until the platform times it out."

	// msgConnectorHubConfig renders hub.ErrConfig. The detail block names the
	// exact variable; this headline places the problem.
	msgConnectorHubConfig = "This Connector's qURL platform endpoint configuration is incomplete or invalid, so it can't start."

	hintConnectorHubConfig = "Hint: production builds ship this configuration built in — install an official qURL release, or for a custom deployment set QURL_CONNECTOR_HUB_HOST, QURL_CONNECTOR_HUB_PORT, and QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 together."

	// Enrollment and platform-assignment renderings for the qurl-go
	// assignment taxonomy. Without these the SDK's own text reaches the
	// terminal, and that text is written for SDK callers: it names Go
	// identifiers and prescribes remedies ("correct WithAgentRuntimeIdentity")
	// that no CLI customer can act on. Each headline says what the platform
	// decided, in the same "platform assignment" vocabulary the refresh
	// messages above already use, and each hint is the one next step.

	// msgConnectorTokenConsumed renders qurl.ErrAssignmentBootstrapConsumed
	// (52108).
	msgConnectorTokenConsumed = "The enrollment token this machine presented has already been used. Enrollment tokens work exactly once."

	// hintConnectorTokenConsumed leads with the fresh-token step, then guards
	// the common second cause: a machine that already enrolled does not need a
	// token at all, and clearing its state directory to "start clean" throws
	// away the identity it earned.
	hintConnectorTokenConsumed = "Hint: create a new enrollment token in the qURL console, set QURL_CONNECTOR_TOKEN to it, and run the command again. If this machine already enrolled once, it doesn't need a token — its Connector identity lives in the state directory, so check --state-dir points there instead of clearing it."

	// msgConnectorTokenRejected renders qurl.ErrAssignmentKeyRejected (52106).
	msgConnectorTokenRejected = "The qURL platform didn't accept this machine's enrollment token."

	// hintConnectorTokenRejected names the causes the customer can actually
	// check — the token is opaque, so none of them are visible locally.
	hintConnectorTokenRejected = "Hint: the token may be mistyped, expired, revoked, or created for a different qURL environment than the endpoint this command is using. Create a new enrollment token in the qURL console, set QURL_CONNECTOR_TOKEN to it, and run the command again."

	// msgConnectorEnrollmentRejected renders
	// qurl.ErrAssignmentRequestRejected (52205 or 52109).
	msgConnectorEnrollmentRejected = "The qURL platform refused this Connector's enrollment request."

	// hintConnectorEnrollmentRejected names the dominant real cause: a token
	// minted for a different Connector than the one --id names.
	hintConnectorEnrollmentRejected = "Hint: this usually means the enrollment token was created for a different Connector than the one --id names. Create a new enrollment token for this Connector in the qURL console, set QURL_CONNECTOR_TOKEN to it, and run the command again."

	// msgConnectorEnrollmentDisabled renders
	// qurl.ErrAssignmentRegistrationDisabled (52107).
	msgConnectorEnrollmentDisabled = "The qURL platform isn't accepting new Connector enrollments here right now."

	// hintConnectorEnrollmentDisabled says the quiet part: this is a
	// platform-side switch, so retrying or re-minting a token changes nothing.
	hintConnectorEnrollmentDisabled = "Hint: enrollment is turned off on the platform side, so no change on this machine — including a new token — will change the answer. Ask your qURL administrator to enable Connector enrollment for this account, then run the command again."

	// msgConnectorIdentityRejected renders
	// qurl.ErrAssignmentIdentityRejected (52201). Distinct from
	// msgConnectorIdentityConflict, which is a purely local disagreement: this
	// one is the platform refusing the identity that was presented.
	msgConnectorIdentityRejected = "The qURL platform refused the Connector identity this machine presented."

	hintConnectorIdentityRejected = "Hint: the stored identity may have been removed from your account, or LAYERV_AGENT_ID may name an identity this account doesn't own. Remove that override if you set it; otherwise enroll this machine again with a new enrollment token and a fresh --state-dir."

	// msgConnectorQuotaExceeded renders qurl.ErrAssignmentQuotaExceeded
	// (52203).
	msgConnectorQuotaExceeded = "Your qURL account has reached its limit on enrolled Connectors, so this machine can't be added."

	hintConnectorQuotaExceeded = "Hint: retire a Connector you no longer use in the qURL console, or ask your qURL administrator to raise the limit, then run the command again."

	// msgConnectorAssignmentUnavailable renders the four sentinels whose
	// customer story and next step are identical — the platform could not
	// place this Connector right now: qurl.ErrAssignmentUnavailable (52200),
	// ErrAssignmentRateLimited (52204), ErrAssignmentReassignmentRequired
	// (52202), and ErrAssignmentRecoveryRequired (the bounded budget ran out).
	// They keep separate exit codes, which is where a script reads the
	// difference.
	msgConnectorAssignmentUnavailable = "The qURL platform couldn't give this Connector its platform assignment right now — it's busy, moving capacity, or briefly unreachable from this machine."

	hintConnectorAssignmentUnavailable = "Hint: this is usually temporary and nothing on this machine needs to change. Run `qurl connector run` again in a few minutes; if it keeps happening, check this machine's outbound network access and contact LayerV support."

	// msgConnectorAssignmentInvalid renders
	// qurl.ErrAssignmentInvalidResponse: the platform answered outside its
	// own contract. Terminal by design — retrying an answer that failed
	// validation could conceal a platform deployment fault.
	msgConnectorAssignmentInvalid = "The qURL platform answered this Connector's enrollment request in a way this version can't accept, so it stopped instead of guessing."

	hintConnectorAssignmentInvalid = "Hint: this is a problem on the qURL platform side, not on this machine, and running the command again won't clear it. Contact LayerV support with the detail above."

	// msgConnectorAssignmentExpired renders qurl.ErrAssignmentLeaseExpired.
	// The Connector normally renews this itself, so reaching the terminal
	// means the renewal did not happen.
	msgConnectorAssignmentExpired = "This Connector's qURL platform assignment has expired and wasn't renewed."

	hintConnectorAssignmentExpired = "Hint: run `qurl connector run` again — a Connector renews its own assignment at startup. If it keeps expiring, check this machine's clock and its outbound network access."
)

// CustomerMessages returns every fixed customer-facing string this package
// can emit, for the CLI-wide jargon gate.
func CustomerMessages() []string {
	return []string{
		errorPrefix,
		msgLinksUnavailable,
		msgNoCredential,
		hintNoCredential,
		hintUnauthorized,
		hintNotFound,
		hintQuotaExceeded,
		hintRetryAfter,
		hintRevoked,
		hintRetired,
		hintScope,
		hintFrozen,
		hintExpired,
		hintKeyInvalid,
		labelKeyring,
		labelCredentialFile,
		labelCRID,
		msgLoggedInAs,
		msgLoggedOut,
		msgNothingStored,
		msgSavedTo,
		msgAlreadyPublished,
		msgPublishFoundExisting,
		msgConnectorServing,
		msgConnectorReachIt,
		msgConnectorTokenRequired,
		hintConnectorTokenRequired,
		msgConnectorIdentityConflict,
		hintConnectorIdentityConflict,
		msgConnectorRefreshApproval,
		hintConnectorRefreshApproval,
		msgConnectorRefreshDisabled,
		msgConnectorRefreshModeInvalid,
		hintConnectorRefreshModeInvalid,
		hintConnectorRefreshDisabled,
		msgConnectorRefreshExhausted,
		hintConnectorRefreshExhausted,
		msgConnectorRetryBudget,
		hintConnectorRetryBudget,
		msgConnectorPreviousSessionActive,
		hintConnectorPreviousSessionActive,
		msgConnectorHubConfig,
		hintConnectorHubConfig,
		msgConnectorTokenConsumed,
		hintConnectorTokenConsumed,
		msgConnectorTokenRejected,
		hintConnectorTokenRejected,
		msgConnectorEnrollmentRejected,
		hintConnectorEnrollmentRejected,
		msgConnectorEnrollmentDisabled,
		hintConnectorEnrollmentDisabled,
		msgConnectorIdentityRejected,
		hintConnectorIdentityRejected,
		msgConnectorQuotaExceeded,
		hintConnectorQuotaExceeded,
		msgConnectorAssignmentUnavailable,
		hintConnectorAssignmentUnavailable,
		msgConnectorAssignmentInvalid,
		hintConnectorAssignmentInvalid,
		msgConnectorAssignmentExpired,
		hintConnectorAssignmentExpired,
	}
}
