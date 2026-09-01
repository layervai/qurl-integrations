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

	msgConnectorStopped  = "This qURL Connector is stopped."
	hintConnectorStopped = "Hint: run `qurl start <CRID>`, then try again."

	// msgNoCredential renders the missing registered-device bootstrap condition.
	msgNoCredential = "This machine is not enrolled with qURL."

	hintNoCredential  = "Hint: run `qurl login`, or set QURL_API_KEY for one-time device enrollment."
	hintUnauthorized  = "Hint: the service rejected this device identity. Run `qurl login` with a current account API key."
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
	hintEnrollmentScope          = "Hint: log in with an API key that includes qurl:agent. The CLI uses it only to mint a one-time device enrollment credential."
	hintConnectorEnrollmentScope = "Hint: this registered device is not allowed to publish local apps. Contact your qURL administrator."

	// hintFrozen is the account-standing message for 403 account_frozen: the
	// key is fine, the account is paused — a materially different situation
	// from a permissions problem, so it must not hide behind a generic
	// forbidden hint.
	hintFrozen = "Hint: your qURL account is frozen, so requests are paused even though your API key is valid. Contact your qURL administrator or LayerV support to restore the account."

	// hintExpired tells an expired key apart from a rejected one: the remedy
	// is a new key, not a retyped one.
	hintExpired = "Hint: this API key has expired. Create a new key in the qURL dashboard and run `qurl login` again."

	// hintKeyInvalid covers the platform's explicit not-a-key answer. Unlike
	// the generic 401 hint it does not steer to QURL_API_KEY: the key in
	// hand — typed at login or stored — is the thing the service refused.
	hintKeyInvalid = "Hint: the qURL service doesn't recognize this API key. Re-copy it from the qURL dashboard, then run `qurl login` again (or update QURL_API_KEY if that's where it lives)."

	// labelCRID prefixes the copyable identity line every document that has
	// a CRID ends with, so publish and the Connector serve note cannot drift
	// into two spellings of the same label.
	labelCRID = "CRID:"

	// msgDeviceEnrolled opens the login confirmation; %s is the account.
	msgDeviceEnrolled = "Enrolled this device for %s."

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

	// msgConnectorConnectionConfig renders native connection configuration
	// errors without exposing deployment topology or custom-build inputs.
	msgConnectorConnectionConfig = "This qURL CLI is missing required built-in connection settings, so local sharing can't start."

	hintConnectorConnectionConfig = "Hint: install or reinstall an official qURL release. For a custom deployment, use the release supplied by your qURL administrator."

	labelConnectorErrorCode = "Error code:"

	msgConnectorSessionConfig  = "This Connector's saved account binding is missing or invalid, so it can't start."
	hintConnectorSessionConfig = "Hint: update to the latest qURL CLI and sign in again. Do not add cloud or database settings, and do not edit the Connector state files."

	msgConnectorDeviceCredential  = "This machine's saved qURL device credential cannot be used safely."
	hintConnectorDeviceCredential = "Hint: keep the local Connector state unchanged and run `qurl login` with a current qURL API key. If the problem continues, wait a short time, retry, and contact LayerV support."

	msgConnectorPeerTimeout  = "The qURL platform did not answer this Connector before the network timeout."
	hintConnectorPeerTimeout = "Hint: keep the local Connector state unchanged, check this machine's outbound network access, and retry after a short wait. Contact LayerV support if the problem continues."

	msgConnectorRecoveryCredentialRejected  = "The qURL platform refused the account credential used to recover this registered device."
	hintConnectorRecoveryCredentialRejected = "Hint: run `qurl login` with a current qURL API key. If qURL accepts that key but recovery is still refused, keep the local Connector state unchanged, wait a short time, retry, and then contact LayerV support if it continues."

	msgConnectorRecoveryIdentityRejected  = "The qURL platform could not verify this registered device for credential recovery."
	hintConnectorRecoveryIdentityRejected = "Hint: keep the local Connector state unchanged and run `qurl login` with the account that owns this device. If the problem continues, contact LayerV support."

	msgConnectorRecoveryRevokeRequired  = "The qURL platform reports that this device credential is still active, so it did not replace it."
	hintConnectorRecoveryRevokeRequired = "Hint: do not delete or edit the local Connector state. Retry once, then contact LayerV support if the platform still reports conflicting device state."

	msgConnectorRecoveryUnavailable  = "The qURL platform could not finish registered-device recovery right now."
	hintConnectorRecoveryUnavailable = "Hint: keep the local Connector state unchanged and run the same command again after a short wait. The CLI will resume the saved recovery safely; contact LayerV support if it continues."

	msgConnectorRecoveryConflict  = "The saved replacement credential conflicts with the platform's current recovery state, so qURL stopped safely."
	hintConnectorRecoveryConflict = "Hint: do not delete or edit the local Connector state. Contact LayerV support before you retry or reprovision this device."

	msgConnectorRecoveryPersistence  = "qURL could not safely save the replacement credential on this machine, so recovery stopped."
	hintConnectorRecoveryPersistence = "Hint: check free disk space and access to the qURL state directory, then retry without deleting or editing the saved Connector state."

	msgConnectorRecoveryInvalid  = "The qURL platform and this CLI did not agree on the registered-device recovery response, so qURL stopped safely."
	hintConnectorRecoveryInvalid = "Hint: update to the latest qURL CLI, keep the local Connector state unchanged, and retry. Contact LayerV support if it continues."

	msgConnectorRecoveryExpired  = "The safe recovery period for this registered device has ended."
	hintConnectorRecoveryExpired = "Hint: keep the local Connector state unchanged and contact LayerV support to recover or deliberately reprovision this device."

	msgConnectorEnrollmentConfig  = "qURL could not start this Connector's device enrollment because the CLI's local enrollment settings are invalid."
	hintConnectorEnrollmentConfig = "Hint: update to the latest qURL CLI, keep the local Connector state unchanged, and retry. Contact LayerV support if the problem continues."

	msgConnectorEnrollmentUnavailable  = "This Connector's device enrollment did not finish before its safe retry period ended."
	hintConnectorEnrollmentUnavailable = "Hint: keep the local Connector state unchanged and run the same command again after a short wait. The CLI will safely resume the saved enrollment."

	msgConnectorEnrollmentIdentity  = "The qURL platform could not verify this Connector during device enrollment."
	hintConnectorEnrollmentIdentity = "Hint: keep the local Connector state unchanged, run `qurl login` with a current API key for the owning account, and retry. Contact LayerV support if it continues."

	msgConnectorEnrollmentConflict  = "This Connector's local identity conflicts with the platform's current device enrollment state, so qURL stopped safely."
	hintConnectorEnrollmentConflict = "Hint: do not delete or edit the local Connector state. Contact LayerV support before you retry or enroll this machine again."

	msgConnectorEnrollmentInvalid  = "The qURL platform refused this Connector's device enrollment request."
	hintConnectorEnrollmentInvalid = "Hint: update to the latest qURL CLI, keep the local Connector state unchanged, and retry. Contact LayerV support if the platform still refuses the request."

	msgConnectorEnrollmentMismatch  = "The qURL platform and this CLI did not agree on this Connector's device enrollment, so qURL stopped safely."
	hintConnectorEnrollmentMismatch = "Hint: update to the latest qURL CLI, keep the local Connector state unchanged, and retry. Contact LayerV support if the problem continues."

	msgConnectorDeviceQuota  = "This qURL account has reached its limit on active device credentials for Connector enrollment."
	hintConnectorDeviceQuota = "Hint: ask your qURL administrator to revoke an unused device credential or raise the limit, then run the command again."

	msgConnectorEnrollmentPersistence  = "qURL could not safely save this Connector's device enrollment state on this machine, so it stopped."
	hintConnectorEnrollmentPersistence = "Hint: keep the local Connector state unchanged, make sure no other qURL command is changing it, and check free disk space and directory access before you retry."

	// Native assigned-cell resource setup. These messages deliberately call
	// the capability a Connector resource, distinct from enrollment and from
	// the longer-lived cell assignment.
	msgConnectorResourceInvalidRequest  = "This Connector's saved resource request is invalid, so it stopped instead of sending or changing it."
	hintConnectorResourceInvalidRequest = "Hint: update to the latest qURL CLI and run the same command again. If it still fails, do not edit the state file; contact LayerV support."

	msgConnectorResourceUnavailable  = "The qURL platform couldn't set up this Connector's resource right now."
	hintConnectorResourceUnavailable = "Hint: run the same command again after a short wait. The CLI saved the exact request and will safely replay it; if the problem persists, check this machine's outbound network access and contact LayerV support."

	msgConnectorResourceEntitlement  = "This Connector identity is not allowed to use the Connector ID you requested."
	hintConnectorResourceEntitlement = "Hint: confirm --id matches the Connector this machine was enrolled to run. To change that identity, deliberately enroll a fresh state directory with a token created for the correct Connector ID; otherwise ask your qURL administrator to grant access."

	msgConnectorResourceConflict  = "This machine's saved Connector resource identity no longer matches the active resource for that Connector ID, so it refused the replacement."
	hintConnectorResourceConflict = "Hint: do not delete or edit the state file just to bypass this check. Confirm whether the resource was deliberately replaced, then contact your qURL administrator or LayerV support before reprovisioning this machine."

	msgConnectorResourceQuota  = "Your qURL account has reached its limit on active Connector resources."
	hintConnectorResourceQuota = "Hint: remove a Connector resource you no longer use with the qURL management tools, or ask your qURL administrator to raise the limit, then run the command again."

	msgConnectorResourceInvalidResponse  = "The qURL platform answered this Connector's resource request in a way this version can't accept, so it stopped instead of guessing."
	hintConnectorResourceInvalidResponse = "Hint: this is a problem on the qURL platform side, not on this machine. Keep the state directory unchanged and contact LayerV support."

	msgConnectorResourceLocalVerification  = "The qURL platform's answer did not match this Connector's saved request or resource identity, so the CLI refused the answer and stopped."
	hintConnectorResourceLocalVerification = "Hint: do not delete or edit the state file to accept a different identity. Contact LayerV support before running this Connector again."

	msgConnectorResourceLocalConflict  = "The qURL platform's answer reused an identity already saved for a different Connector ID, so the CLI kept the earlier identity and stopped."
	hintConnectorResourceLocalConflict = "Hint: confirm each Connector uses its intended --id and state directory. Do not edit the state file to bypass this check; contact your qURL administrator or LayerV support."

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

	hintConnectorQuotaExceeded = "Hint: retire a Connector you no longer use in the qURL dashboard, or ask your qURL administrator to raise the limit, then run the command again."

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
		msgConnectorStopped,
		hintConnectorStopped,
		msgNoCredential,
		hintNoCredential,
		hintUnauthorized,
		hintNotFound,
		hintQuotaExceeded,
		hintRetryAfter,
		hintRevoked,
		hintRetired,
		hintScope,
		hintEnrollmentScope,
		hintFrozen,
		hintExpired,
		hintKeyInvalid,
		labelCRID,
		msgDeviceEnrolled,
		msgSavedTo,
		msgAlreadyPublished,
		msgPublishFoundExisting,
		msgConnectorConnectionConfig,
		hintConnectorConnectionConfig,
		labelConnectorErrorCode,
		msgConnectorSessionConfig,
		hintConnectorSessionConfig,
		msgConnectorDeviceCredential,
		hintConnectorDeviceCredential,
		msgConnectorPeerTimeout,
		hintConnectorPeerTimeout,
		msgConnectorRecoveryCredentialRejected,
		hintConnectorRecoveryCredentialRejected,
		msgConnectorRecoveryIdentityRejected,
		hintConnectorRecoveryIdentityRejected,
		msgConnectorRecoveryRevokeRequired,
		hintConnectorRecoveryRevokeRequired,
		msgConnectorRecoveryUnavailable,
		hintConnectorRecoveryUnavailable,
		msgConnectorRecoveryConflict,
		hintConnectorRecoveryConflict,
		msgConnectorRecoveryPersistence,
		hintConnectorRecoveryPersistence,
		msgConnectorRecoveryInvalid,
		hintConnectorRecoveryInvalid,
		msgConnectorRecoveryExpired,
		hintConnectorRecoveryExpired,
		msgConnectorEnrollmentConfig,
		hintConnectorEnrollmentConfig,
		msgConnectorEnrollmentUnavailable,
		hintConnectorEnrollmentUnavailable,
		msgConnectorEnrollmentIdentity,
		hintConnectorEnrollmentIdentity,
		msgConnectorEnrollmentConflict,
		hintConnectorEnrollmentConflict,
		msgConnectorEnrollmentInvalid,
		hintConnectorEnrollmentInvalid,
		msgConnectorEnrollmentMismatch,
		hintConnectorEnrollmentMismatch,
		msgConnectorDeviceQuota,
		hintConnectorDeviceQuota,
		msgConnectorEnrollmentPersistence,
		hintConnectorEnrollmentPersistence,
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
