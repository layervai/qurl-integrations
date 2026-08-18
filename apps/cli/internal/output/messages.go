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

	// msgLoggedInAs opens the login confirmation; %s is the account.
	msgLoggedInAs = "Logged in as %s."

	// msgLoggedOut confirms removal; %s lists the storage that held the key.
	msgLoggedOut = "Logged out. Removed your qURL API key from the %s."

	// msgNothingStored is logout's idempotent no-op note (still exit 0).
	msgNothingStored = "No qURL API key is stored on this machine; nothing to remove."

	// msgSavedTo confirms a completed download: destination, then size.
	msgSavedTo = "Saved to %s (%d bytes)."

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

	// msgConnectorHubConfig renders hub.ErrConfig. The detail block names the
	// exact variable; this headline places the problem.
	msgConnectorHubConfig = "This Connector's qURL platform endpoint configuration is incomplete or invalid, so it can't start."

	hintConnectorHubConfig = "Hint: production builds ship this configuration built in — install an official qURL release, or for a custom deployment set QURL_CONNECTOR_HUB_HOST, QURL_CONNECTOR_HUB_PORT, and QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 together."
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
		msgLoggedInAs,
		msgLoggedOut,
		msgNothingStored,
		msgSavedTo,
		msgConnectorTokenRequired,
		hintConnectorTokenRequired,
		msgConnectorIdentityConflict,
		hintConnectorIdentityConflict,
		msgConnectorRefreshApproval,
		hintConnectorRefreshApproval,
		msgConnectorRefreshDisabled,
		hintConnectorRefreshDisabled,
		msgConnectorRefreshExhausted,
		hintConnectorRefreshExhausted,
		msgConnectorRetryBudget,
		hintConnectorRetryBudget,
		msgConnectorHubConfig,
		hintConnectorHubConfig,
	}
}
