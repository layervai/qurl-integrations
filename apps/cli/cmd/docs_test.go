package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDocsCommandGeneratesStableMarkdown(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()

	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"docs", "markdown", "--output-dir", dir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("docs markdown: %v\n%s", err, buf.String())
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open docs root: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close docs root: %v", err)
		}
	})
	content, err := root.ReadFile("qurl_create.md")
	if err != nil {
		t.Fatalf("read generated create doc: %v", err)
	}
	text := string(content)
	for _, want := range []string{"TARGET_URL", "qurl create https://api.example.com/data"} {
		if !strings.Contains(text, want) {
			t.Errorf("generated doc missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Auto generated") {
		t.Errorf("generated docs should not include Cobra date footer:\n%s", text)
	}
	if strings.HasSuffix(text, "\n\n") {
		t.Errorf("generated docs should be trimmed to a single trailing newline")
	}
}

func TestTrimMarkdownDocsOnlyRewritesMarkdown(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open docs root: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close docs root: %v", err)
		}
	})
	if err := root.WriteFile("qurl.md", []byte("# qurl\n\n\n"), 0o600); err != nil {
		t.Fatalf("write markdown: %v", err)
	}
	if err := root.WriteFile("notes.txt", []byte("keep\n\n\n"), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}

	if err := trimMarkdownDocs(dir); err != nil {
		t.Fatalf("trimMarkdownDocs: %v", err)
	}

	gotMD, err := root.ReadFile("qurl.md")
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if string(gotMD) != "# qurl\n" {
		t.Errorf("markdown = %q", gotMD)
	}
	gotTXT, err := root.ReadFile("notes.txt")
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	if string(gotTXT) != "keep\n\n\n" {
		t.Errorf("text file should not be rewritten: %q", gotTXT)
	}
}

func TestSetDisableAutoGenRestoresCommandTree(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	child := &cobra.Command{Use: "child", DisableAutoGenTag: true}
	root.AddCommand(child)

	restore := setDisableAutoGen(root, true)
	if !root.DisableAutoGenTag || !child.DisableAutoGenTag {
		t.Fatalf("expected root and child AutoGen tags disabled")
	}

	restore()
	if root.DisableAutoGenTag {
		t.Errorf("root DisableAutoGenTag should be restored to false")
	}
	if !child.DisableAutoGenTag {
		t.Errorf("child DisableAutoGenTag should be restored to true")
	}
}
