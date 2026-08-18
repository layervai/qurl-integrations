// Package consume turns a verified resolve answer into the thing the user
// asked for: the resource open in their browser, or its bytes on disk.
//
// The action decision (Decide) is local and runs before any network traffic:
// a terminal gets the browser, --file gets a download, and piped output with
// no --file is refused outright — the CLI never launches a browser nobody
// can see (§16.2).
//
// Phase-1 downloads fetch the verified resolved link directly over HTTP(S).
// The SDK's programmatic opener (qurl.EnterPortal) was studied first and
// deliberately not used: opening a qv2 link that way requires deployment
// trust configuration — issuer keys plus a relay allowlist or cell catalog —
// and qurl-go v0.5.3 ships its embedded deployment.json empty ({"issuers":
// [], "cells": [], "relay_allowlist": []}), with no LayerV-published
// deployment file to point QURL_DEPLOYMENT at. Every EnterPortal call would
// therefore refuse (ErrNotConfigured wrapping ErrNoDeployment) before
// reaching the network, for every link, on every machine. When the SDK
// ships real deployment trust material, Downloader.fetch is the seam to
// revisit. Until then the browser path — which carries the full link,
// fragment included — is the primary qv2 consumption surface, and --file
// serves the resolved URL's bytes as delivered.
//
// Nothing here talks to the qURL API and nothing here carries the API
// credential: the download client is a plain HTTP client, so the bearer key
// can never leak to the link host. Every link this package acts on has
// already passed the CLI's CRID verification (cmd.verifyResolved), and the
// re-resolve a mid-download retry performs goes through the same verifying
// closure.
package consume

import "errors"

// Action is what one `qurl get` invocation will do, decided locally before
// any network traffic.
type Action int

// The three Phase-1 actions.
const (
	// ActionOpenBrowser opens the verified link in the user's browser.
	// Chosen only when stdout is a terminal and --file was not given.
	ActionOpenBrowser Action = iota
	// ActionSaveFile downloads the resource to the --file path atomically.
	ActionSaveFile
	// ActionStreamStdout streams the raw resource bytes to stdout
	// (--file -), with no decoration on any stream.
	ActionStreamStdout
)

// Fixed customer-facing messages. The sentinel errors carry them verbatim,
// so what the user reads and what the exit-code table matches on are the
// same values; the CLI-wide jargon gate asserts over CustomerMessages.
const (
	// MsgPipedNeedsFile is the §16.2 refusal: no browser without a terminal.
	MsgPipedNeedsFile = "this output is piped, and a browser is only opened on a terminal. Add --file <path> to download the file (--file - streams the raw bytes), or use `qurl resolve` to print an access link"

	// MsgFileExists is the overwrite refusal; wraps add the path and remedy.
	MsgFileExists = "the destination already exists"

	// MsgLinkExpired reports that the access link expired and the one
	// automatic refresh expired too.
	MsgLinkExpired = "the access link expired before the download finished, even after requesting a fresh one — try again"

	// MsgLinkFetch frames a link host that answered but refused to serve;
	// wraps add the HTTP status.
	MsgLinkFetch = "the download failed"

	// The fixed fragments wraps append to MsgFileExists.
	msgForceRemedy   = "pass --force to replace it"
	msgDirectoryDest = "it is a directory — give a file path instead"
)

// Sentinel errors, each mapped to exactly one exit code in
// internal/exitcode (the AST tripwire there refuses any sentinel without a
// row).
var (
	// ErrPipedNeedsFile refuses a piped `qurl get` with no --file (usage).
	ErrPipedNeedsFile = errors.New(MsgPipedNeedsFile)
	// ErrFileExists refuses to replace an existing destination without
	// --force (conflict).
	ErrFileExists = errors.New(MsgFileExists)
	// ErrLinkExpired reports expiry that survived the single retry (the
	// platform's gone family).
	ErrLinkExpired = errors.New(MsgLinkExpired)
	// ErrLinkFetch reports a link host answer outside the download contract
	// (server error).
	ErrLinkFetch = errors.New(MsgLinkFetch)
)

// Decide picks the action from local facts only: stdout's TTY-ness and the
// --file flag. It never touches the network, so a refusal here is free.
func Decide(outIsTTY bool, file string) (Action, error) {
	switch {
	case file == "-":
		return ActionStreamStdout, nil
	case file != "":
		return ActionSaveFile, nil
	case outIsTTY:
		return ActionOpenBrowser, nil
	default:
		return 0, ErrPipedNeedsFile
	}
}

// CustomerMessages returns every fixed customer-facing string this package
// can emit, for the CLI-wide jargon gate.
func CustomerMessages() []string {
	return []string{
		MsgPipedNeedsFile,
		MsgFileExists,
		MsgLinkExpired,
		MsgLinkFetch,
		msgForceRemedy,
		msgDirectoryDest,
	}
}
