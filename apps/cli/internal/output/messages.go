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
	}
}
