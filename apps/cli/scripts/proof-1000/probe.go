package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	probeTimeout        = 90 * time.Second
	probeContentTimeout = 30 * time.Second

	classAPIRollover = "api-rollover"
	classACDeny      = "nhp-ac-deny"
	classContent404  = "content-404"
	classOther       = "other"
)

// fetchDiagnosis is what the visitor access path answered when asked
// directly through the SDK, bypassing the CLI's customer-language mapping:
// the raw NHP deny code, an overload signal, or the HTTP status the granted
// content URL returned. It is the only way to see the deny code — the CLI
// keeps it private by design.
type fetchDiagnosis struct {
	At            time.Time `json:"at"`
	DenyCode      string    `json:"deny_code,omitempty"`
	Overloaded    bool      `json:"overloaded,omitempty"`
	Granted       bool      `json:"granted"`
	ContentStatus int       `json:"content_http_status,omitempty"`
	OpenSeconds   uint32    `json:"open_seconds,omitempty"`
	Detail        string    `json:"detail,omitempty"`
}

// probeAccess mints a share link for crid through the consume CLI, then
// opens it with the SDK's own opener (honoring QURL_DEPLOYMENT from this
// process's environment) and, when access is granted, requests the content
// URL once to record its HTTP status.
func probeAccess(ctx context.Context, env *environment, crid string) *fetchDiagnosis {
	d := &fetchDiagnosis{At: time.Now()}
	share := runCLI(ctx, env.ConsumeBin, env.childEnv, probeTimeout, "share", crid, "--quiet")
	if share.ExitCode != cliExitOK {
		d.Detail = "share link mint failed (exit " + itoa(share.ExitCode) + "): " + env.redactor.apply(lastErrorLine(share.Stderr))
		return d
	}
	link := strings.TrimSpace(share.Stdout)
	if link == "" {
		d.Detail = "share printed no link"
		return d
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	handle, err := qurl.EnterPortal(probeCtx, link)
	if err != nil {
		var deny *qurl.ServerDenyError
		switch {
		case errors.As(err, &deny):
			d.DenyCode = deny.ErrCode
		case errors.Is(err, qurl.ErrServerOverloaded):
			d.Overloaded = true
		default:
			d.Detail = env.redactor.apply(err.Error())
		}
		return d
	}
	d.Granted, d.OpenSeconds = true, handle.OpenSeconds
	d.ContentStatus, d.Detail = fetchContentStatus(ctx, handle)
	return d
}

func fetchContentStatus(ctx context.Context, handle *qurl.ResourceHandle) (status int, detail string) {
	reqCtx, cancel := context.WithTimeout(ctx, probeContentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, handle.ResourceURL, http.NoBody)
	if err != nil {
		return 0, err.Error()
	}
	if err := handle.AuthorizeContentRequest(req); err != nil {
		return 0, "authorize content request: " + err.Error()
	}
	resp, err := (&http.Client{Timeout: probeContentTimeout}).Do(req)
	if err != nil {
		return 0, "content request: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, ""
}

// classifyFailure names the failure class the report groups by. A declared
// api-rollover window wins; then the probed deny code; then the CLI's own
// wording when no probe ran (the CLI says "busy" only for 52005, 52028, or
// an overload signal).
func classifyFailure(window, errText string, diag *fetchDiagnosis) string {
	if strings.HasPrefix(window, classAPIRollover) {
		return classAPIRollover
	}
	if diag != nil {
		switch {
		case diag.DenyCode != "":
			return classACDeny + ":" + diag.DenyCode
		case diag.Overloaded:
			return classACDeny + ":overloaded"
		case diag.Granted && diag.ContentStatus == http.StatusNotFound:
			return classContent404
		}
	}
	lower := strings.ToLower(errText)
	switch {
	case strings.Contains(lower, "busy"):
		return classACDeny + ":unprobed"
	case strings.Contains(lower, "http 404"):
		return classContent404
	default:
		return classOther
	}
}

// runProbe is the --probe mode: one CRID, one SDK-level answer, as JSON.
func runProbe(ctx context.Context, env *environment, crid string, stdout io.Writer) int {
	d := probeAccess(ctx, env, crid)
	raw, _ := json.MarshalIndent(d, "", "  ")
	_, _ = fmt.Fprintln(stdout, string(raw))
	if d.Granted && d.ContentStatus == http.StatusOK {
		return exitOK
	}
	return exitProofFailed
}
