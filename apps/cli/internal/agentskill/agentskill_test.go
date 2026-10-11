package agentskill

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// skillWith is a skill file whose frontmatter carries the given lines after
// the description.
func skillWith(extra string) string {
	return "---\nname: qurl\ndescription: Share an app (\"share my app\"): private, by CRID.\n" + extra + "---\n\n# qURL\n"
}

func versioned(version int) string {
	return skillWith("metadata:\n  version: \"" + strconv.Itoa(version) + "\"\n")
}

func TestVersion(t *testing.T) {
	tests := []struct {
		name string
		file string
		want int
	}{
		{"quoted", skillWith("metadata:\n  version: \"7\"\n"), 7},
		{"single quoted", skillWith("metadata:\n  version: '7'\n"), 7},
		{"bare", skillWith("metadata:\n  version: 7\n"), 7},
		{"two digits", skillWith("metadata:\n  version: \"12\"\n"), 12},
		{"after another metadata key", skillWith("metadata:\n  author: LayerV\n  version: \"3\"\n"), 3},
		{"tab indent", skillWith("metadata:\n\tversion: \"3\"\n"), 3},
		{"windows line ends", strings.ReplaceAll(versioned(4), "\n", "\r\n"), 4},
		{"byte order mark", "\xef\xbb\xbf" + versioned(4), 4},
		{"frontmatter still open at the end of what was read", "---\nname: qurl\nmetadata:\n  version: \"5\"\n", 5},

		{"no version", skillWith(""), 0},
		{"no frontmatter", "# qURL\n\nmetadata:\n  version: \"9\"\n", 0},
		{"empty file", "", 0},
		{"version outside metadata", skillWith("version: \"9\"\n"), 0},
		{"version under another map", skillWith("compatibility:\n  version: \"9\"\nmetadata:\n  author: LayerV\n"), 0},
		{"version in the body", skillWith("") + "metadata:\n  version: \"9\"\n", 0},
		{"version in the description", "---\nname: qurl\ndescription: metadata: version: \"9\"\n---\n", 0},
		{"a date", skillWith("metadata:\n  version: \"2026-10-11\"\n"), 0},
		{"a release number", skillWith("metadata:\n  version: \"1.2\"\n"), 0},
		{"negative", skillWith("metadata:\n  version: \"-3\"\n"), 0},
		{"a word", skillWith("metadata:\n  version: latest\n"), 0},
		{"empty", skillWith("metadata:\n  version:\n"), 0},
		{"one quote", skillWith("metadata:\n  version: \"\n"), 0},
		{"too large to be a number", skillWith("metadata:\n  version: \"99999999999999999999999\"\n"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Version([]byte(tc.file)); got != tc.want {
				t.Errorf("Version = %d, want %d\n%s", got, tc.want, tc.file)
			}
		})
	}
}

func save(t *testing.T, home, folder, content string) string {
	t.Helper()
	name := filepath.Join(home, filepath.FromSlash(folder), "skills", "qurl", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

const claudeCommand = "mkdir -p ~/.claude/skills/qurl && curl -fsSL https://layerv.ai/skills/qurl/SKILL.md -o ~/.claude/skills/qurl/SKILL.md"

func TestFindOutdated(t *testing.T) {
	t.Run("no copy is not a finding", func(t *testing.T) {
		if found, ok := FindOutdated(t.TempDir(), "darwin"); ok {
			t.Errorf("found %+v with no copy saved", found)
		}
	})
	t.Run("no home directory is not a finding", func(t *testing.T) {
		if found, ok := FindOutdated("", "darwin"); ok {
			t.Errorf("found %+v with no home directory", found)
		}
	})
	t.Run("a copy at the oldest version this release works with is current", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", versioned(MinVersion))
		if found, ok := FindOutdated(home, "darwin"); ok {
			t.Errorf("found %+v, want nothing", found)
		}
	})
	t.Run("a newer copy is current", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", versioned(MinVersion+1))
		if found, ok := FindOutdated(home, "darwin"); ok {
			t.Errorf("found %+v, want nothing", found)
		}
	})
	t.Run("a copy with no version is older", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", skillWith(""))
		found, ok := FindOutdated(home, "darwin")
		if !ok {
			t.Fatal("found nothing, want the copy with no version")
		}
		if found.Path != "~/.claude/skills/qurl/SKILL.md" {
			t.Errorf("Path = %q", found.Path)
		}
		// The command the skill has an agent keep in its saved note, to the byte.
		if found.ReplaceCommand != claudeCommand {
			t.Errorf("ReplaceCommand = %q\nwant            %q", found.ReplaceCommand, claudeCommand)
		}
	})
	t.Run("a copy below the oldest version is older", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", versioned(MinVersion-1))
		if _, ok := FindOutdated(home, "linux"); !ok {
			t.Error("found nothing, want the old copy")
		}
	})
	t.Run("every agent folder is looked in, and the command names that folder", func(t *testing.T) {
		for _, folder := range agentFolders {
			home := t.TempDir()
			save(t, home, folder, skillWith(""))
			found, ok := FindOutdated(home, "linux")
			if !ok {
				t.Errorf("%s: found nothing", folder)
				continue
			}
			want := "~/" + folder + "/skills/qurl/SKILL.md"
			if found.Path != want {
				t.Errorf("%s: Path = %q, want %q", folder, found.Path, want)
			}
			wantCommand := "mkdir -p ~/" + folder + "/skills/qurl && curl -fsSL " + SourceURL + " -o " + want
			if found.ReplaceCommand != wantCommand {
				t.Errorf("%s: ReplaceCommand = %q, want %q", folder, found.ReplaceCommand, wantCommand)
			}
		}
	})
	t.Run("Claude Code's folder is among them", func(t *testing.T) {
		if agentFolders[0] != ".claude" {
			t.Errorf("agentFolders[0] = %q, want .claude: the skill's own command saves there", agentFolders[0])
		}
	})
	t.Run("a current copy does not hide an old one in another folder", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", versioned(MinVersion))
		save(t, home, ".codex", skillWith(""))
		found, ok := FindOutdated(home, "linux")
		if !ok || found.Path != "~/.codex/skills/qurl/SKILL.md" {
			t.Errorf("found %+v (%v), want the copy in .codex", found, ok)
		}
	})
	t.Run("one copy is named, the first in order", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".codex", skillWith(""))
		save(t, home, ".claude", skillWith(""))
		found, _ := FindOutdated(home, "linux")
		if found.Path != "~/.claude/skills/qurl/SKILL.md" {
			t.Errorf("Path = %q, want the copy in .claude", found.Path)
		}
	})
	t.Run("a folder where the file should be is not a copy", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "qurl", "SKILL.md"), 0o700); err != nil {
			t.Fatal(err)
		}
		if found, ok := FindOutdated(home, "linux"); ok {
			t.Errorf("found %+v for a folder", found)
		}
	})
	t.Run("a version beyond what is read is not found", func(t *testing.T) {
		home := t.TempDir()
		save(t, home, ".claude", "---\nname: qurl\ndescription: "+strings.Repeat("x", headLimit)+"\nmetadata:\n  version: \"9\"\n---\n")
		if _, ok := FindOutdated(home, "linux"); !ok {
			t.Error("a version past the read limit counted; the read is not bounded")
		}
	})
	t.Run("Windows gets the full path and a command its shells run", func(t *testing.T) {
		home := t.TempDir()
		full := save(t, home, ".claude", skillWith(""))
		found, ok := FindOutdated(home, "windows")
		if !ok {
			t.Fatal("found nothing")
		}
		if found.Path != full {
			t.Errorf("Path = %q, want %q", found.Path, full)
		}
		if want := `curl.exe -fsSL ` + SourceURL + ` -o "` + full + `"`; found.ReplaceCommand != want {
			t.Errorf("ReplaceCommand = %q, want %q", found.ReplaceCommand, want)
		}
	})
}

func TestACopyThisProcessMayNotReadIsNotAFinding(t *testing.T) {
	if os.Geteuid() <= 0 {
		t.Skip("needs a user that file permissions apply to")
	}
	home := t.TempDir()
	name := save(t, home, ".claude", skillWith(""))
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	if found, ok := FindOutdated(home, "linux"); ok {
		t.Errorf("found %+v for a file that cannot be read", found)
	}
}
