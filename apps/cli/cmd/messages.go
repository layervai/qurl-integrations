package main

// exampleCRID is a structurally valid production CRID used in help text so
// examples look exactly like real usage (60 characters, lowercase, 'a'
// first character = production).
const exampleCRID = "aea6x7mea52zcalolw7nis3g4iy3rcfr7nzyfukkuujsqufnxhmvhhtledfa"

// Fixed customer-facing message constants. They are defined once here and
// referenced by both the commands and the jargon-gate test, so the strings a
// customer sees and the strings the gate vets can never drift apart.
const (
	// msgKeyringUnavailable is the file-fallback warning: emitted by login
	// when the key had to be stored in the fallback file, and once per
	// invocation by any command that reads the key from that file.
	msgKeyringUnavailable = "OS keyring storage isn't available on this system; your qURL API key is kept in a file only your user can read (mode 0600)"

	// msgVerifyMismatch is printed (stderr only) when a resolve response
	// fails CRID verification; nothing is emitted on stdout and the exit
	// code is 12.
	msgVerifyMismatch = "the service's answer did not match the CRID you asked for, so the link was discarded and nothing was printed. Try again; if it keeps happening, stop and contact whoever shared the CRID with you"

	// msgVerifyMissing covers a resolve response that carried nothing to
	// verify against; same fail-closed contract as msgVerifyMismatch.
	msgVerifyMissing = "the service's answer carried no CRID to verify against, so the link was discarded and nothing was printed. This endpoint may be too old for verified resolution"

	// msgNeedsYes is the non-interactive guard for destructive commands.
	msgNeedsYes = "confirmation required: re-run with --yes (interactive confirmation needs a terminal)"

	// msgDeleteCanceled acknowledges a declined confirmation prompt.
	msgDeleteCanceled = "Canceled — nothing was deleted."

	// msgNoCRIDReturned warns when publish succeeds but the service minted
	// no CRID (older deployments).
	msgNoCRIDReturned = "The service did not return a CRID for this resource; use the resource ID shown above until it does."

	// msgInsecureEndpoint warns that a plain-http non-loopback endpoint
	// sends the API key unencrypted. Loopback endpoints never warn.
	msgInsecureEndpoint = "your API key would travel unencrypted: %s uses plain http on a non-local address — use https"

	// msgTTLClamped reports the service granting a shorter link lifetime
	// than requested.
	msgTTLClamped = "Note: the service granted a %s link lifetime instead of the requested %s."

	// msgNoKeyProvided is login's empty-input error.
	msgNoKeyProvided = "no API key provided"

	// msgAlreadyGone notes an idempotent delete that had nothing left to do.
	msgAlreadyGone = "It was already deleted; nothing left to do."

	// msgAlreadyPublished notes that the service returned the existing
	// resource for an already-published target.
	msgAlreadyPublished = "This target was already published; showing the existing resource."

	// msgOpeningBrowser is get's browser-mode note, printed after the
	// verified link and before the launcher runs.
	msgOpeningBrowser = "Opening it in your browser..."

	// msgBrowserFailed reports a launcher that would not start; the link is
	// already on stdout by then, so the user is not stranded.
	msgBrowserFailed = "couldn't open your browser: %w. The access link is printed above — open it yourself, or re-run with --file to download the file"

	// msgFileNeedsPath refuses an explicitly empty --file value.
	msgFileNeedsPath = "--file needs a path, or \"-\" to stream to stdout"

	// msgFileDashJSON refuses combining raw-byte output with the JSON
	// document format.
	msgFileDashJSON = "--file - streams the raw file bytes to stdout and can't be combined with --output json"

	// msgBrowserJSON refuses browser-open under the JSON output mode: a
	// machine asked for data, and a spawned browser is not data.
	msgBrowserJSON = "browser opening isn't available with --output json — use --file to download, or `qurl resolve --output json` for the link"

	// msgConnectorIDRequired refuses connector run without a Connector ID.
	msgConnectorIDRequired = "--id is required: pass your Connector's ID — the route name your app serves under — or set connector_id in your profile"

	// msgConnectorIDConflict refuses --id and its deprecated --slug alias
	// disagreeing; the first %q is --id's value, the second the alias's. It
	// deliberately names the deprecated flag the customer just typed — the
	// only jargon-gate-exempt string (see jargonExemptMessages) — and is
	// removed at the next major together with the alias itself.
	msgConnectorIDConflict = "--id and --slug disagree (%q vs %q): --slug is a deprecated alias of --id, so pass only --id"

	// msgConnectorTargetRequired refuses connector run without a local app.
	msgConnectorTargetRequired = "--target is required: the local app to serve, as host:port (\":8080\" means 127.0.0.1:8080)"

	// msgConnectorTargetInvalid refuses a --target that is not host:port.
	msgConnectorTargetInvalid = "--target %q is not host:port (\":8080\" means 127.0.0.1:8080)"

	// msgConnectorRefreshModeInvalid refuses a --refresh-mode outside the
	// three modes. The same misspelling in the environment variable is a
	// configuration error instead; only the flag is a usage error.
	msgConnectorRefreshModeInvalid = "--refresh-mode must be manual, auto, or disabled; got %q"

	// msgConnectorServing announces the serve loop on stderr; the Connector
	// ID (as the platform records it), then the local target being served.
	msgConnectorServing = "Starting Connector %q for your local app at %s. Press Ctrl-C to stop."

	// msgConnectorStopped acknowledges a graceful signal-initiated stop.
	msgConnectorStopped = "Stopped."
)

// customerMessages returns every fixed customer-facing string the cmd
// package can emit, for the jargon gate.
func customerMessages() []string {
	return []string{
		msgKeyringUnavailable,
		msgVerifyMismatch,
		msgVerifyMissing,
		msgNeedsYes,
		msgDeleteCanceled,
		msgNoCRIDReturned,
		msgTTLClamped,
		msgNoKeyProvided,
		msgAlreadyGone,
		msgAlreadyPublished,
		msgOpeningBrowser,
		msgBrowserFailed,
		msgFileNeedsPath,
		msgFileDashJSON,
		msgBrowserJSON,
		msgConnectorIDRequired,
		msgConnectorIDConflict,
		msgConnectorTargetRequired,
		msgConnectorTargetInvalid,
		msgConnectorRefreshModeInvalid,
		msgConnectorServing,
		msgConnectorStopped,
	}
}
