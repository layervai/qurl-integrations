package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/auth"
)

func TestHelpSourceUsesExplicitCommandTokens(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "handler.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "userHelpMessage" && fn.Name.Name != "adminHelpMessage") {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(value, "/qurl") {
				t.Errorf("%s authors a raw command instead of an explicit token: %s", fn.Name.Name, value)
			}
			return true
		})
	}
}

func TestStandaloneRepliesUseInvokedCommand(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	for _, command := range []string{"/qurl", "/qurl-sandbox"} {
		t.Run(command, func(t *testing.T) {
			reply := slashReply(t, h, command, "create https://example.com")
			if !strings.Contains(reply, "`"+command+" get ") || !strings.Contains(reply, "`"+command+" list`") {
				t.Errorf("deprecation names another command: %s", reply)
			}
			response := httptest.NewRecorder()
			h.handleGet(response, url.Values{fieldCommand: {command}, fieldText: {"list"}})
			if !strings.Contains(response.Body.String(), command+" help") {
				t.Errorf("unknown get fallback names another command: %s", response.Body.String())
			}
		})
	}
	// Unknown subcommands must not invent a sibling admin command.
	if reply := slashReply(t, h, "/qurl-bogus", "not-a-command"); strings.Contains(reply, "/qurl-bogus-admin") {
		t.Errorf("invented admin pointer: %s", reply)
	}
}

func TestSetupFailureUsesInvokedCommand(t *testing.T) {
	for _, command := range []string{"/qurl", "/qurl-sandbox", "/qurl-sandbox-admin", ""} {
		want := userCommandName(command)
		got := authErrorMessage(auth.ErrWorkspaceNotConfigured, command)
		if !strings.Contains(got, "`"+want+" setup <email>`") {
			t.Errorf("command %q: %s", command, got)
		}
	}
}
