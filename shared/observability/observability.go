// Package observability provides telemetry setup and log redaction for qURL integrations.
//
// Log redaction is a bounded backstop that mirrors Discord's current logger
// policy: matched string-like scalar values are blanked, containers are walked
// by inner field names, non-string map keys are not interpreted, and values
// beyond the depth cap pass through unchanged.
package observability
