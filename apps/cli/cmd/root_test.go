package main

import (
	"errors"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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
