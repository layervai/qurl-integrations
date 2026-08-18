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

	hintNoCredential  = "Hint: set QURL_API_KEY, or run `qurl login` once it can store keys for you."
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
	}
}
