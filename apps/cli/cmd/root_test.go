package main

import (
	"errors"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestEnvOrFlag(t *testing.T) {
	t.Run("flag takes precedence", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		flag := "flag-key"
		got := envOrFlag("QURL_API_KEY", &flag)
		if got != "flag-key" {
			t.Errorf("got %q, want %q", got, "flag-key")
		}
	})

	t.Run("falls back to env", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		empty := ""
		got := envOrFlag("QURL_API_KEY", &empty)
		if got != "env-key" {
			t.Errorf("got %q, want %q", got, "env-key")
		}
	})

	t.Run("nil flag falls back to env", func(t *testing.T) {
		t.Setenv("QURL_API_KEY", "env-key")
		got := envOrFlag("QURL_API_KEY", nil)
		if got != "env-key" {
			t.Errorf("got %q, want %q", got, "env-key")
		}
	})

	t.Run("returns empty when neither set", func(t *testing.T) {
		empty := ""
		got := envOrFlag("QURL_NONEXISTENT_VAR", &empty)
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestNewClient_MissingAPIKey(t *testing.T) {
	empty := ""
	_, err := newClient(&empty, &empty)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		t.Fatalf("expected *cliError, got %T", err)
	}
}

func TestGetFormatter(t *testing.T) {
	jsonFmt := "json"
	tableFmt := "table"

	f := getFormatter(&jsonFmt)
	if _, ok := f.(output.JSONFormatter); !ok {
		t.Errorf("expected JSONFormatter for 'json', got %T", f)
	}

	f = getFormatter(&tableFmt)
	if _, ok := f.(output.TableFormatter); !ok {
		t.Errorf("expected TableFormatter for 'table', got %T", f)
	}

	f = getFormatter(nil)
	if _, ok := f.(output.TableFormatter); !ok {
		t.Errorf("expected TableFormatter for nil, got %T", f)
	}
}
