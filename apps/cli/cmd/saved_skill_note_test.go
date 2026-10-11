package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/agentskill"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// The note for a saved copy of the qURL agent skill that is older than this
// release works with: which commands say it, where it goes, and that stdout
// is the same with it and without it.

// savedSkillNote is the whole of what stderr carries for an old copy in
// Claude Code's folder. The command in it is, to the byte, the one the skill
// has an agent keep in its saved note.
const savedSkillNote = "The qURL skill saved at ~/.claude/skills/qurl/SKILL.md is older than this release of the qURL CLI works with. To replace it, run:\n" +
	"  mkdir -p ~/.claude/skills/qurl && curl -fsSL https://layerv.ai/skills/qurl/SKILL.md -o ~/.claude/skills/qurl/SKILL.md\n"

// homeWithSkill returns a home directory whose Claude Code folder holds a
// skill file with the given lines in its frontmatter.
func homeWithSkill(t *testing.T, frontmatter string) string {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("the note is written for a POSIX shell here; the package's own tests cover the other form")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "qurl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := "---\nname: qurl\ndescription: Share an app.\n" + frontmatter + "---\n\n# qURL\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func homeWithOldSkill(t *testing.T) string { return homeWithSkill(t, "") }

func homeWithCurrentSkill(t *testing.T) string {
	return homeWithSkill(t, "metadata:\n  version: \""+strings.Repeat("9", 3)+"\"\n")
}

// savedSkillCommands are the invocations that say the note: the ones the
// skill has an agent run. Each returns its arguments for one mock server.
var savedSkillCommands = []struct {
	name string
	args func(srv *apitest.Server, flags ...string) []string
}{
	{"--version", func(_ *apitest.Server, flags ...string) []string { return append(flags, "--version") }},
	{"version", func(_ *apitest.Server, flags ...string) []string { return append(flags, "version") }},
	{"publish", func(srv *apitest.Server, flags ...string) []string {
		return append(append([]string{"--endpoint", srv.URL}, flags...), "publish", "https://example.com/data")
	}},
}

func TestAnOldSavedSkillIsNotedOnStderrAndStdoutIsUnchanged(t *testing.T) {
	for _, command := range savedSkillCommands {
		for _, form := range []struct {
			name  string
			flags []string
		}{
			{"text", nil},
			{"json", []string{"-o", "json"}},
		} {
			t.Run(command.name+" "+form.name, func(t *testing.T) {
				srv := apitest.NewServer(t)
				without := runCLI(t, &runOpts{args: command.args(srv, form.flags...), home: t.TempDir()})
				with := runCLI(t, &runOpts{args: command.args(srv, form.flags...), home: homeWithOldSkill(t)})
				if with.code != 0 || without.code != 0 {
					t.Fatalf("exit = %d and %d, want 0; stderr: %s", with.code, without.code, with.stderr.String())
				}
				if got, want := with.stdout.String(), without.stdout.String(); got != want || got == "" {
					t.Errorf("stdout changed with the note:\n got %q\nwant %q", got, want)
				}
				if strings.Contains(without.stderr.String(), "skill") {
					t.Errorf("stderr names a skill with no copy saved: %q", without.stderr.String())
				}
				// The note is the only thing the copy adds, and it is whole.
				added, ok := withoutOnce(with.stderr.String(), savedSkillNote)
				if !ok {
					t.Fatalf("stderr = %q\nwant the note %q", with.stderr.String(), savedSkillNote)
				}
				if added != without.stderr.String() {
					t.Errorf("stderr beside the note = %q, want %q", added, without.stderr.String())
				}
			})
		}
	}
}

// withoutOnce removes the one occurrence of part from s.
func withoutOnce(s, part string) (string, bool) {
	if strings.Count(s, part) != 1 {
		return s, false
	}
	return strings.Replace(s, part, "", 1), true
}

func TestPublishNotesAnOldSavedSkillBeforeItPublishes(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "not a target"}, home: homeWithOldSkill(t)})
	if res.code == 0 {
		t.Fatalf("a publish of no target succeeded: %s", res.stdout.String())
	}
	if !strings.HasPrefix(res.stderr.String(), savedSkillNote) {
		t.Errorf("stderr = %q, want it to start with the note", res.stderr.String())
	}
	if len(srv.Requests()) != 0 {
		t.Errorf("the refused publish sent %d requests", len(srv.Requests()))
	}
}

func TestQuietSaysNothingOfAnOldSavedSkill(t *testing.T) {
	for _, command := range savedSkillCommands {
		t.Run(command.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			without := runCLI(t, &runOpts{args: command.args(srv, "--quiet"), home: t.TempDir()})
			with := runCLI(t, &runOpts{args: command.args(srv, "--quiet"), home: homeWithOldSkill(t)})
			if with.code != 0 {
				t.Fatalf("exit = %d; stderr: %s", with.code, with.stderr.String())
			}
			if with.stdout.String() != without.stdout.String() || with.stderr.String() != without.stderr.String() {
				t.Errorf("--quiet changed with an old copy:\nstdout %q, want %q\nstderr %q, want %q",
					with.stdout.String(), without.stdout.String(), with.stderr.String(), without.stderr.String())
			}
		})
	}
}

func TestACurrentSavedSkillGetsNoNote(t *testing.T) {
	for _, command := range savedSkillCommands {
		t.Run(command.name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: command.args(apitest.NewServer(t)), home: homeWithCurrentSkill(t)})
			if res.code != 0 || strings.Contains(res.stderr.String(), "skill") {
				t.Errorf("exit = %d, stderr = %q, want no note for a current copy", res.code, res.stderr.String())
			}
		})
	}
}

// Only the commands the skill has an agent run say the note: a script that
// lists or opens resources is not what an agent runs before a publish.
func TestOtherCommandsSayNothingOfAnOldSavedSkill(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, args := range [][]string{
		{"--endpoint", srv.URL, "list"},
		{"--help"},
		{"publish", "--help"},
	} {
		res := runCLI(t, &runOpts{args: args, home: homeWithOldSkill(t)})
		if res.code != 0 || strings.Contains(res.stderr.String(), "skill") {
			t.Errorf("qurl %s: exit = %d, stderr = %q, want no note", strings.Join(args, " "), res.code, res.stderr.String())
		}
	}
}

// The note goes through the printer, which masks what looks like a
// credential. The command in it must come out as it went in, in both forms:
// a reader runs it as printed.
func TestTheNoteIsPrintedAsItIsWritten(t *testing.T) {
	home := homeWithOldSkill(t)
	for _, goos := range []string{"linux", "windows"} {
		saved, found := agentskill.FindOutdated(home, goos)
		if !found {
			t.Fatalf("%s: no old copy found", goos)
		}
		var stderr bytes.Buffer
		printer := output.New(&output.Streams{Out: io.Discard, Err: &stderr}, output.FormatText, false, false, false, nil)
		printer.Notef(msgSavedSkillOutdated, saved.Path, saved.ReplaceCommand)
		if want := fmt.Sprintf(msgSavedSkillOutdated, saved.Path, saved.ReplaceCommand) + "\n"; stderr.String() != want {
			t.Errorf("%s: printed %q, want %q", goos, stderr.String(), want)
		}
	}
}

func TestAHomeDirectoryThatCannotBeFoundIsNotAnError(t *testing.T) {
	root, opts := newRoot("test", discardStreams(), func(g *globalOpts) {
		g.userHomeDir = func() (string, error) { return "", errors.New("no home directory") }
	})
	root.SetArgs([]string{"--version"})
	if code := run(t.Context(), root, opts); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// The real process looks under the real home directory, and nothing else
// does: newRoot alone leaves the lookup off, so a test that builds the tree
// cannot read the skill of the person who runs the suite.
func TestOnlyTheRealProcessLooksForASavedSkill(t *testing.T) {
	_, opts := newRoot("test", discardStreams())
	if opts.userHomeDir != nil {
		t.Error("newRoot looks under a home directory without being told to")
	}
	lookForSavedSkill(opts)
	if opts.userHomeDir == nil {
		t.Fatal("lookForSavedSkill did not turn the lookup on")
	}
	home, err := opts.userHomeDir()
	wantHome, wantErr := os.UserHomeDir()
	if home != wantHome || (err == nil) != (wantErr == nil) {
		t.Errorf("userHomeDir = %q, %v; want %q, %v", home, err, wantHome, wantErr)
	}
}

// The first release that reads the version works with every versioned skill.
// A release that raises this must ship after the skill it names is served.
func TestTheOldestSkillThisReleaseWorksWith(t *testing.T) {
	if agentskill.MinVersion != 1 {
		t.Errorf("MinVersion = %d; raising it is a change of what the CLI prints for every saved copy below it: ship the skill with that version at %s first, then update this test", agentskill.MinVersion, agentskill.SourceURL)
	}
}
