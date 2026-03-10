package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
	"github.com/layervai/qurl-integrations/shared/client"
)

func TestResolveValue(t *testing.T) {
	t.Run("flag takes precedence over env", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		flag := "flag-key"
		got := resolveValue("QURL_API_KEY", &flag, "api_key", nil)
		if got != "flag-key" {
			t.Errorf("got %q, want %q", got, "flag-key")
		}
	})

	t.Run("env takes precedence when flag empty", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		empty := ""
		got := resolveValue("QURL_API_KEY", &empty, "api_key", nil)
		if got != "env-key" {
			t.Errorf("got %q, want %q", got, "env-key")
		}
	})

	t.Run("nil flag falls back to env", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		got := resolveValue("QURL_API_KEY", nil, "api_key", nil)
		if got != "env-key" {
			t.Errorf("got %q, want %q", got, "env-key")
		}
	})

	t.Run("returns empty when neither set", func(t *testing.T) {
		empty := ""
		got := resolveValue("QURL_NONEXISTENT_VAR", &empty, "nonexistent_key", nil)
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestNewClient_MissingAPIKey(t *testing.T) {
	t.Setenv("QURL_API_KEY", "")
	opts := &globalOpts{}
	_, err := opts.newClient()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		t.Fatalf("expected *cliError, got %T", err)
	}
}

func TestGetFormatter(t *testing.T) {
	opts := &globalOpts{format: "json"}
	f := opts.formatter()
	if _, ok := f.(output.JSONFormatter); !ok {
		t.Errorf("expected JSONFormatter for 'json', got %T", f)
	}

	opts.format = "table"
	f = opts.formatter()
	if _, ok := f.(output.TableFormatter); !ok {
		t.Errorf("expected TableFormatter for 'table', got %T", f)
	}

	opts.format = ""
	f = opts.formatter()
	if _, ok := f.(output.TableFormatter); !ok {
		t.Errorf("expected TableFormatter for empty, got %T", f)
	}
}

func TestFormatError_PlainError(t *testing.T) {
	err := errors.New("something went wrong")
	got := formatError(err)
	if got != "something went wrong" {
		t.Errorf("got %q, want %q", got, "something went wrong")
	}
}

func TestFormatError_APIError(t *testing.T) {
	err := &client.APIError{
		StatusCode: 404,
		Title:      "Not Found",
		Detail:     "QURL not found",
		RequestID:  "req_123",
	}
	got := formatError(err)
	if !strings.Contains(got, "Not Found") {
		t.Errorf("expected 'Not Found' in output: %s", got)
	}
	if !strings.Contains(got, "404") {
		t.Errorf("expected '404' in output: %s", got)
	}
	if !strings.Contains(got, "req_123") {
		t.Errorf("expected request ID in output: %s", got)
	}
}

func TestFormatError_APIError401Hint(t *testing.T) {
	err := &client.APIError{
		StatusCode: 401,
		Title:      "Unauthorized",
	}
	got := formatError(err)
	if !strings.Contains(got, "API key") {
		t.Errorf("expected API key hint in output: %s", got)
	}
}

func TestFormatError_APIError429(t *testing.T) {
	err := &client.APIError{
		StatusCode: 429,
		Title:      "Too Many Requests",
		RetryAfter: 30,
	}
	got := formatError(err)
	if !strings.Contains(got, "30") {
		t.Errorf("expected retry-after value in output: %s", got)
	}
}

func TestFormatError_QuotaExceeded(t *testing.T) {
	err := &client.APIError{
		StatusCode: 403,
		Title:      "Forbidden",
		Code:       "quota_exceeded",
	}
	got := formatError(err)
	if !strings.Contains(got, "pricing") {
		t.Errorf("expected pricing hint in output: %s", got)
	}
}

func TestFormatError_InvalidFields(t *testing.T) {
	err := &client.APIError{
		StatusCode: 422,
		Title:      "Validation Error",
		InvalidFields: map[string]string{
			"target_url": "must be a valid URL",
			"expires_in": "invalid format",
		},
	}
	got := formatError(err)
	if !strings.Contains(got, "target_url") || !strings.Contains(got, "expires_in") {
		t.Errorf("expected field names in output: %s", got)
	}
}

func TestFormatError_WrappedAPIError(t *testing.T) {
	apiErr := &client.APIError{
		StatusCode: 404,
		Title:      "Not Found",
	}
	wrapped := fmt.Errorf("create QURL: %w", apiErr)
	got := formatError(wrapped)
	if !strings.Contains(got, "create QURL") {
		t.Errorf("expected wrapping context in output: %s", got)
	}
	if !strings.Contains(got, "Not Found") {
		t.Errorf("expected API error title in output: %s", got)
	}
}
