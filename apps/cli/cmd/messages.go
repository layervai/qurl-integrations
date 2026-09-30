package main

// exampleCRID is a structurally valid production CRID used in help text so
// examples look exactly like real usage (60 characters, lowercase, 'a'
// first character = production).
const exampleCRID = "aea6x7mea52zcalolw7nis3g4iy3rcfr7nzyfukkuujsqufnxhmvhhtledfa"

// Fixed customer-facing message constants. They are defined once here and
// referenced by both the commands and the jargon-gate test, so the strings a
// customer sees and the strings the gate vets can never drift apart.
const (
	// msgVerifyMismatch is printed (stderr only) when a share response
	// fails CRID verification; nothing is emitted on stdout and the exit
	// code is 12.
	msgVerifyMismatch = "the service's answer did not match the CRID you asked for, so the link was discarded and nothing was printed. Try again; if it keeps happening, stop and contact whoever shared the CRID with you"

	// msgVerifyMissing covers a share response that carried nothing to
	// verify against; same fail-closed contract as msgVerifyMismatch.
	msgVerifyMissing = "the service's answer carried no CRID to verify against, so the link was discarded and nothing was printed. Try again; if it keeps happening, contact qURL support"

	// msgNeedsYes is the non-interactive guard for destructive commands.
	msgNeedsYes = "confirmation required: re-run with --yes (interactive confirmation needs a terminal)"

	// msgDeleteCanceled acknowledges a declined confirmation prompt.
	msgDeleteCanceled = "Canceled — nothing was deleted."

	// msgInsecureEndpoint warns that a plain-http non-loopback endpoint sends
	// an authorization credential unencrypted. Loopback endpoints never warn.
	msgInsecureEndpoint = "your authorization credential would travel unencrypted: %s uses plain http on a non-local address — use https"

	// msgDevicePublicKeyUnreadable reports that whoami could not read the local
	// device state for its public key. The rest of the identity still prints
	// and the command still succeeds.
	msgDevicePublicKeyUnreadable = "could not read the local device public key: %v"
	// msgKeyStorageUnrecognized warns that the local state's key storage is not
	// one this version names, so whoami -o json omits it rather than inventing
	// a value.
	msgKeyStorageUnrecognized = "the local device state uses a key storage this version does not recognize"
	// msgTPMSealedDevice and msgTPMSealedLinuxDevice are shown once, just
	// before a new device identity is sealed to the TPM under native
	// supervision: the moment the choice becomes permanent is the only moment
	// it is cheap to change. Linux adds the tss requirement of the background
	// job.
	msgTPMSealedDevice      = "Local device state is being sealed to this machine's TPM, so it will open only on this machine. To keep it unencrypted instead, move the state directory aside afterwards and run the command again with LAYERV_KEY_PROVIDER=file; that creates a new device identity."
	msgTPMSealedLinuxDevice = "Local device state is being sealed to this machine's TPM, so it will open only on this machine. The background job needs the tss group as well: if you joined it this session, log out fully first. To keep it unencrypted instead, move the state directory aside afterwards and run the command again with LAYERV_KEY_PROVIDER=file; that creates a new device identity."
	// Key storage descriptions whoami shows for the providers it names; the
	// other connector providers are shown by their own id.
	msgKeyStorageTPM             = "TPM (sealed to this machine)"
	msgKeyStorageFile            = "file (owner-only, not encrypted)"
	msgKeyStorageUnrecognizedRow = "unrecognized provider"

	// msgDevicePublicKeyInvalid reports that the local device state loaded but
	// holds no usable public key. The bad value itself is never printed.
	msgDevicePublicKeyInvalid = "the local device state has no valid public key: %v"

	// msgTTLClamped reports the service granting a shorter link lifetime
	// than requested.
	msgTTLClamped = "Note: the service granted a %s link lifetime instead of the requested %s."

	// msgNoKeyProvided is login's empty-input error.
	msgNoKeyProvided = "no API key provided"

	// msgAlreadyGone notes an idempotent delete that had nothing left to do.
	msgAlreadyGone = "It was already deleted; nothing left to do."

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
	msgBrowserJSON = "browser opening isn't available with --output json — use --file to download, or `qurl share --output json` for the link"
)

// customerMessages returns every fixed customer-facing string the cmd
// package can emit, for the jargon gate.
func customerMessages() []string {
	return []string{
		msgAccountSetup,
		msgAccountContinue,
		msgAccountLinked,
		msgAccountRecovered,
		msgAnonymousDevice,
		msgAnonymousSupervisedDevice,
		msgDevicePublicKeyUnreadable,
		msgKeyStorageUnrecognized,
		msgTPMSealedDevice,
		msgTPMSealedLinuxDevice,
		msgKeyStorageTPM,
		msgKeyStorageFile,
		msgKeyStorageUnrecognizedRow,
		msgDevicePublicKeyInvalid,
		msgAccountChooseOwner,
		msgAccountOwnerDenied,
		msgVerifyMismatch,
		msgVerifyMissing,
		msgNeedsYes,
		msgDeleteCanceled,
		msgTTLClamped,
		msgNoKeyProvided,
		msgAlreadyGone,
		msgOpeningBrowser,
		msgBrowserFailed,
		msgFileNeedsPath,
		msgFileDashJSON,
		msgBrowserJSON,
	}
}

const (
	msgAccountContinue  = "Continue in your browser to recover resource access."
	msgAccountSetup     = "This device controls your qURL resources.\n\nLink an account to recover access and manage resources from other devices.\nYour existing links will keep working.\nLinking is permanent. Check the account you choose in the browser.\n\nContinue in your browser."
	msgAccountLinked    = "Account linked. Your existing links are unchanged."
	msgAccountRecovered = "Resource access recovered on this device. Existing links are unchanged."
	msgAnonymousDevice  = "Using a new device identity. Keep its local state, or run qurl account setup to enable recovery."
	// msgAnonymousSupervisedDevice replaces msgAnonymousDevice under external
	// supervision, where linking goes through the supervising app.
	msgAnonymousSupervisedDevice = "Using a new device identity. Keep its local state; the supervising app can link an account to enable recovery."
	msgAccountChooseOwner        = "choose resources to recover with --owner; available owners: %s"
	msgAccountOwnerDenied        = "this account does not own the selected resources"
)
