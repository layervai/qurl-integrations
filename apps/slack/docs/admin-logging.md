# Slack IDs in admin and owner logs

Use `internal/slacklog.ID` for Slack team, enterprise, channel, user and target
IDs in structured admin/owner logs. Use `slacklog.IDs` for a group of workspace
IDs. Apply this at the log call, including before signature or owner checks.
Never change the IDs passed to authorization, storage, Slack, or user replies.

The helper keeps empty values and uppercase ASCII letters/digits up to 64 bytes.
Other values become `invalid-slack-owner-id-len-N`, where N is the byte length.
This marker preserves the existing Web API adapter log contract. Do not truncate
and print a malformed ID: it can contain credentials, control characters or
unrelated personal data. If Slack changes its ID alphabet, review this allowlist.

This is log formatting, not identity validation or anonymization. Valid IDs stay
readable so operators can correlate rejected and successful admin actions. A
signed Slack request does not prove that every stored or supplied ID is well
formed. Do not put identifiers in metric dimensions.

The current callers cover membership/ownership admin handlers, setup and
uninstall owner gates, the CRID admin denial, and the workspace Web API adapters.
For admin/owner Slack identifier fields, use `slacklog.ID` rather than the
free-form `sanitizeLogValue` helper. Keep `sanitizeLogValue` for free-form text;
keep API error text under its separate existing redaction rules. Older non-admin
surfaces are outside this change; use this convention when adding or changing an
admin/owner ID log there.

The CRID denial previously escaped/truncated malformed IDs; it now uses the
length-only marker. Valid correlation values and the log message name stay the
same.
