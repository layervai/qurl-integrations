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
	hintScope                    = "Hint: your API key isn't allowed to request access links. Ask your qURL administrator for a key with resolve access."
	hintConnectorEnrollmentScope = "Hint: log in with an API key that includes qurl:agent. The CLI uses it only to mint a one-shot Connector enrollment credential."

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

	// msgConnectorHubConfig renders hub.ErrConfig. The detail block names the
	// exact variable; this headline places the problem.
	msgConnectorHubConfig = "This Connector's qURL platform endpoint configuration is incomplete or invalid, so it can't start."

	hintConnectorHubConfig = "Hint: production builds ship this configuration built in — install an official qURL release, or for a custom deployment set QURL_CONNECTOR_HUB_HOST, QURL_CONNECTOR_HUB_PORT, and QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 together."

	// Native assigned-cell resource setup. These messages deliberately call
	// the capability a Connector resource, distinct from enrollment and from
	// the longer-lived cell assignment.
	msgConnectorResourceInvalidRequest  = "This Connector's saved resource request is invalid, so it stopped instead of sending or changing it."
	hintConnectorResourceInvalidRequest = "Hint: update to the latest qURL CLI and run the same command again. If it still fails, do not edit the state file; contact LayerV support with the detail above."

	msgConnectorResourceUnavailable  = "The qURL platform couldn't set up this Connector's resource right now."
	hintConnectorResourceUnavailable = "Hint: run the same command again after a short wait. The CLI saved the exact request and will safely replay it; if the problem persists, check this machine's outbound network access and contact LayerV support."

	msgConnectorResourceEntitlement  = "This Connector identity is not allowed to use the Connector ID you requested."
	hintConnectorResourceEntitlement = "Hint: confirm --id matches the Connector this machine was enrolled to run. To change that identity, deliberately enroll a fresh state directory with a token created for the correct Connector ID; otherwise ask your qURL administrator to grant access."

	msgConnectorResourceConflict  = "This machine's saved Connector resource identity no longer matches the active resource for that Connector ID, so it refused the replacement."
	hintConnectorResourceConflict = "Hint: do not delete or edit the state file just to bypass this check. Confirm whether the resource was deliberately replaced, then contact your qURL administrator or LayerV support before reprovisioning this machine."

	msgConnectorResourceQuota  = "Your qURL account has reached its limit on active Connector resources."
	hintConnectorResourceQuota = "Hint: remove a Connector resource you no longer use with the qURL management tools, or ask your qURL administrator to raise the limit, then run the command again."

	msgConnectorResourceInvalidResponse  = "The qURL platform answered this Connector's resource request in a way this version can't accept, so it stopped instead of guessing."
	hintConnectorResourceInvalidResponse = "Hint: this is a problem on the qURL platform side, not on this machine. Keep the state directory unchanged and contact LayerV support with the detail above."

	msgConnectorResourceLocalVerification  = "The qURL platform's answer did not match this Connector's saved request or resource identity, so the CLI refused the answer and stopped."
	hintConnectorResourceLocalVerification = "Hint: do not delete or edit the state file to accept a different identity. Contact LayerV support with the detail above before running this Connector again."

	msgConnectorResourceLocalConflict  = "The qURL platform's answer reused an identity already saved for a different Connector ID, so the CLI kept the earlier identity and stopped."
	hintConnectorResourceLocalConflict = "Hint: confirm each Connector uses its intended --id and state directory. Do not edit the state file to bypass this check; contact your qURL administrator or LayerV support with the detail above."

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

	// The v2 CLI mints enrollment credentials through the signed-in account. It
	// never accepts a customer-supplied bootstrap token on the command line or
	// through a legacy environment alias.
	hintConnectorTokenConsumed = "Hint: run the command again so qURL can mint a fresh enrollment credential. Do not delete the local state directory; if this repeats, sign in again with `qurl login` and contact LayerV support."

	// msgConnectorTokenRejected renders qurl.ErrAssignmentKeyRejected (52106).
	msgConnectorTokenRejected = "The qURL platform didn't accept this machine's enrollment token."

	// The credential is minted by the CLI from the active account and endpoint;
	// the customer should correct that authenticated context, not inject a token.
	hintConnectorTokenRejected = "Hint: confirm this command uses the intended qURL endpoint, sign in again with `qurl login`, and retry. If the platform still rejects the new credential, contact LayerV support."

	// msgConnectorEnrollmentRejected renders
	// qurl.ErrAssignmentRequestRejected (52205 or 52109).
	msgConnectorEnrollmentRejected = "The qURL platform refused this Connector's enrollment request."

	// The dominant customer-correctable cause is an explicit identity that the
	// active account cannot enroll. Credential minting itself is automatic.
	hintConnectorEnrollmentRejected = "Hint: confirm --id names a Connector this account can use, or omit --id to use the stable ID qURL generates for this machine and local app. Then run the command again."

	// msgConnectorEnrollmentDisabled renders
	// qurl.ErrAssignmentRegistrationDisabled (52107).
	msgConnectorEnrollmentDisabled = "The qURL platform isn't accepting new Connector enrollments here right now."

	// hintConnectorEnrollmentDisabled says the quiet part: this is a
	// platform-side switch, so retrying or re-minting a token changes nothing.
	hintConnectorEnrollmentDisabled = "Hint: enrollment is turned off on the platform side, so no change on this machine — including a new token — will change the answer. Ask your qURL administrator to enable Connector enrollment for this account, then run the command again."

	// msgConnectorIdentityRejected renders
	// qurl.ErrAssignmentIdentityRejected (52201): the platform refused the
	// identity that was presented.
	msgConnectorIdentityRejected = "The qURL platform refused the Connector identity this machine presented."

	hintConnectorIdentityRejected = "Hint: the stored identity may have been removed from your account, or QURL_CONNECTOR_AGENT_ID may name an identity this account doesn't own. Remove that override if you set it, sign in again with `qurl login`, and retry. Do not edit or delete the local state files."

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

	hintConnectorAssignmentUnavailable = "Hint: this is usually temporary and nothing on this machine needs to change. Run the same command again in a few minutes; if it keeps happening, check this machine's outbound network access and contact LayerV support."

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

	hintConnectorAssignmentExpired = "Hint: run the same command again — a Connector renews its own assignment at startup. If it keeps expiring, check this machine's clock and its outbound network access."
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
		msgConnectorHubConfig,
		hintConnectorHubConfig,
		msgConnectorResourceInvalidRequest,
		hintConnectorResourceInvalidRequest,
		msgConnectorResourceUnavailable,
		hintConnectorResourceUnavailable,
		msgConnectorResourceEntitlement,
		hintConnectorResourceEntitlement,
		msgConnectorResourceConflict,
		hintConnectorResourceConflict,
		msgConnectorResourceQuota,
		hintConnectorResourceQuota,
		msgConnectorResourceInvalidResponse,
		hintConnectorResourceInvalidResponse,
		msgConnectorResourceLocalVerification,
		hintConnectorResourceLocalVerification,
		msgConnectorResourceLocalConflict,
		hintConnectorResourceLocalConflict,
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
