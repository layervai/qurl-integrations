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
	msgTPMSealedDevice      = "Local device state will be sealed to this machine's TPM, so it will open only on this machine. To keep it unencrypted instead, move the state directory aside afterwards and run the command again with LAYERV_KEY_PROVIDER=file; that creates a new device identity."
	msgTPMSealedLinuxDevice = "Local device state will be sealed to this machine's TPM, so it will open only on this machine. The background job needs the tss group as well: if you joined it this session, log out fully first. To keep it unencrypted instead, move the state directory aside afterwards and run the command again with LAYERV_KEY_PROVIDER=file; that creates a new device identity."
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

	// msgSessionDurationNotApplied tells a `qurl get --session-duration` user
	// that the flag had no effect, because the link was given for the CRID
	// alone and not minted by this device. The link is still used.
	msgSessionDurationNotApplied = "Note: --session-duration applies only on the resource owner's devices and on devices the publisher allowed. This link uses the resource's own session lifetime."

	// msgValidCRIDRequired refuses an operand that is not a CRID.
	msgValidCRIDRequired = "a valid CRID is required; copy it from the resource listing"

	// msgCRIDLinkRefusalCode is the --verbose diagnostic for a link request
	// with only a CRID that the service refused. It carries the service's
	// code, which the message for the user never does.
	msgCRIDLinkRefusalCode = "< CRID link request refused, code %s"

	// msgCRIDLinkNotSent is the --verbose diagnostic for a link request with
	// only a CRID that was never sent, because the SDK will not ask for a
	// link for that CRID. %s is one word for the class of the cause, from
	// consume.CRIDNotRequestableClass. The message for the user does not say
	// which class it was, and on a device with an identity it is the share
	// request's own answer, which says nothing about this request at all.
	msgCRIDLinkNotSent = "> CRID link request not sent, the SDK will not ask for this CRID: %s"

	// msgShareNeedsDevice frames a failure to open this device's identity on
	// the share path, which share and get both take. The cause follows it
	// unchanged, so the exit code stays the cause's.
	msgShareNeedsDevice = "sharing needs this device's identity: %w"

	// msgPublicGrantsNoEffect follows a grant change on a public resource.
	// Device grants decide who else may use a private resource; a public one
	// does not consult them.
	msgPublicGrantsNoEffect = "This resource is public, so device grants have no effect on it. They apply to a private resource, which is what `qurl publish` creates unless you pass --public."

	// Usage errors for the publish flags that say who can open the resource.
	// msgPublicAndPrivate and msgPrivateFalse are about the hidden --private
	// flag, which is accepted only where it asks for what a publish does
	// anyway.
	msgPublicAndPrivate   = "--private and --public cannot be used together"
	msgPrivateFalse       = "--private=false is not accepted: a resource is private unless you publish it with --public"
	msgAllowKeyWithPublic = "--allow-device-key cannot be used with --public: a public resource has no list of allowed devices"

	// Usage errors of `qurl grants`. msgGrantsReplaceRemoved answers the flag
	// that replaced the complete list, and names what to use instead.
	msgGrantsReplaceRemoved = "--allow-device-key no longer replaces the list: use --add <public-key> to allow a device, --remove <public-key> to take one off the list, or --clear to take every device off it"
	msgGrantsClearWithEdit  = "--clear cannot be combined with --add or --remove"
	msgGrantsAddAndRemove   = "the same public key cannot be given to both --add and --remove"

	// msgGrantsRemoveInvalid refuses a --remove value that is neither of the
	// two things the flag takes.
	msgGrantsRemoveInvalid = "--remove takes public keys and device ids of the form xxxx-xxxx-xxxx-xxxx, each given once"

	// Usage errors of the commands for access requests.
	msgAllowRequestsWithPublic  = "--allow-requests cannot be used with --public: access requests are for a private resource, and a public one already opens for anyone who has the CRID"
	msgRequestsOnAndOff         = "--on and --off cannot be used together"
	msgRequestsSettingNeedsCRID = "--on and --off need the CRID of the resource: qurl requests <CRID> --on"
	// msgRequestCodeInvalid refuses an operand that can never be a request
	// code, before any request is sent.
	msgRequestCodeInvalid = "a request code is six digits, written as 123456, 123 456 or 123-456"

	// `qurl request` and `qurl requests` differ by one letter and do unrelated
	// things. Each one's usage error names the other where a person who typed
	// the wrong name lands.
	hintMeantRequests = "Hint: if you meant to see who asked for access to your resources, use `qurl requests`."
	hintMeantRequest  = "Hint: if you meant to make a request for a supervising app, use `qurl request METHOD PATH`."

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

	// msgPublisherSetNeedsName refuses an empty name: removing the name is a
	// separate, explicit command.
	msgPublisherSetNeedsName = "a publisher name can't be empty — run `qurl publisher clear` to remove the name"

	// msgPublisherNameRefusedReason reports a name the service (or the local
	// gate, for a name that can never be valid) did not accept; %s is the
	// reason. The name itself is never echoed.
	msgPublisherNameRefusedReason = "that publisher name can't be used: %s"
	// msgPublisherUnsupported explains a qURL endpoint that has no publisher
	// profile yet, instead of the generic not-found hint about CRIDs.
	msgPublisherUnsupported = "this qURL endpoint doesn't support publisher names yet. Until it does, your CRIDs show \"no name provided\""
	// msgPublisherNameRefused is the same outcome when no reason was given.
	msgPublisherNameRefused = "that publisher name can't be used. Names are 1 to 64 characters: letters, digits, single spaces, and common punctuation"
	// msgPublisherClearRefusedReason reports a removal the service refused;
	// %s is the reason. `qurl publisher clear` takes no name, so it does not
	// borrow the sentence about a name that can't be used.
	msgPublisherClearRefusedReason = "the publisher name can't be removed: %s"
	// msgPublisherClearRefused is the same outcome when no reason was given.
	msgPublisherClearRefused = "the publisher name can't be removed. Run `qurl publisher` to see the name shown with your CRIDs"
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
		msgSessionDurationNotApplied,
		msgValidCRIDRequired,
		msgShareNeedsDevice,
		msgPublicGrantsNoEffect,
		msgPublicAndPrivate,
		msgPrivateFalse,
		msgAllowKeyWithPublic,
		msgGrantsReplaceRemoved,
		msgGrantsClearWithEdit,
		msgGrantsAddAndRemove,
		msgGrantsRemoveInvalid,
		msgAllowRequestsWithPublic,
		msgRequestsOnAndOff,
		msgRequestsSettingNeedsCRID,
		msgRequestCodeInvalid,
		hintMeantRequests,
		hintMeantRequest,
		msgNoKeyProvided,
		msgAlreadyGone,
		msgOpeningBrowser,
		msgBrowserFailed,
		msgFileNeedsPath,
		msgFileDashJSON,
		msgBrowserJSON,
		msgPublisherSetNeedsName,
		msgPublisherNameRefusedReason,
		msgPublisherNameRefused,
		msgPublisherClearRefusedReason,
		msgPublisherClearRefused,
		msgPublisherUnsupported,
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
