package cridux

// Fixed customer-facing messages, defined once so the CLI's jargon gate can
// assert over every string this package may put in front of a user. Follow
// the §17.1 anatomy: say what looks wrong, then what to do about it, in
// plain language.
const (
	// MsgTypo is the checksum/shape warning. The value is still forwarded —
	// the service has the final say — so the message explains the suspicion
	// without blocking.
	MsgTypo = "This CRID appears to contain a typo. Double-check it against the original; sending it anyway in case it is newer than this tool."

	// MsgAlphabetHint accompanies MsgTypo when the input contains digits the
	// CRID alphabet excludes.
	MsgAlphabetHint = "The characters 0, 1, 8, and 9 never appear in a CRID — check for mistyped o, l, b, or g."

	// MsgTestOnProduction is the refusal shown when a test CRID targets the
	// production endpoint without --yes.
	MsgTestOnProduction = "this is a test CRID, but the CLI is pointed at the production qURL service. Re-run with --yes to send it anyway."

	// MsgTestOnProductionAnyway is the warning when --yes overrides the
	// refusal above.
	MsgTestOnProductionAnyway = "Sending a test CRID to the production qURL service because --yes was given."

	// MsgProductionOnOther warns (and proceeds) when a production CRID is
	// sent to a non-production endpoint.
	MsgProductionOnOther = "This is a production CRID, but the CLI is pointed at a non-production endpoint. The result may not be what you expect."
)

// Messages returns every fixed customer-facing string this package can emit,
// for the CLI-wide jargon gate.
func Messages() []string {
	return []string{
		MsgTypo,
		MsgAlphabetHint,
		MsgTestOnProduction,
		MsgTestOnProductionAnyway,
		MsgProductionOnOther,
	}
}
