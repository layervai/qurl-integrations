// Package agentskill finds a copy of the qURL agent skill that a coding agent
// saved on this machine, and says whether it is older than this release
// works with.
//
// An agent saves the skill once and reads that copy in every later session,
// so the copy never changes by itself. The CLI is what the skill has the
// agent run before every publish, and the CLI is updated through its own
// installer, so it is the one place that can say "your copy is old". It only
// reads: it sends nothing, downloads nothing and changes no file.
package agentskill

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// MinVersion is the oldest version of the skill this release works with. A
// saved copy with a lower version, or with no version line at all, gets the
// note. Raise it in the release that needs a newer skill, and only after
// that skill is the one served at SourceURL: the note's command downloads
// from there, and a download that is still too old would leave the note in
// place for good.
const MinVersion = 1

// SourceURL is where the current skill is served.
const SourceURL = "https://layerv.ai/skills/qurl/SKILL.md"

// skillFile is the skill's place inside an agent's folder.
const skillFile = "skills/qurl/SKILL.md"

// agentFolders are the folders, under the home directory, that hold a coding
// agent's skills. The skill tells an agent to save it in `~/.claude/skills`
// and tells any other agent to put its own skills folder in that place, so
// these are the folders a copy lands in: Claude Code's, the folder several
// agents share, and those of the other agents that load skills from a
// folder of their own. A project's folder is not here, because the skill
// never has an agent save it there.
var agentFolders = []string{
	".claude",
	".agents",
	".codex",
	".gemini",
	".cursor",
	".copilot",
	".config/opencode",
}

// headLimit bounds how much of a copy is read. The version stands in the
// frontmatter, which opens the file and is far shorter than this.
const headLimit = 8 << 10

// Copy is one saved copy of the skill that is older than MinVersion.
type Copy struct {
	// Path is where the copy is, written the way the reader's shell takes
	// it: from `~` where the shell expands that, in full on Windows.
	Path string
	// ReplaceCommand is the one command that puts the current skill there.
	ReplaceCommand string
}

// FindOutdated returns the first saved copy under home that is older than
// MinVersion, in the order of agentFolders. It reports false when no copy is
// saved, when every copy is current, and when a copy cannot be read: a
// person who uses the CLI by hand has no skill, and a file this process may
// not read is not a reason to say anything.
func FindOutdated(home, goos string) (Copy, bool) {
	if home == "" {
		return Copy{}, false
	}
	for _, folder := range agentFolders {
		location := path.Join(folder, skillFile)
		head, err := readHead(filepath.Join(home, filepath.FromSlash(location)))
		if err != nil {
			continue
		}
		if Version(head) >= MinVersion {
			continue
		}
		return describe(home, location, goos), true
	}
	return Copy{}, false
}

// describe writes the copy's path and its replace command for one platform.
// For Claude Code's folder on a POSIX shell the command is, to the byte, the
// one the skill has an agent keep in its saved note, so an agent can see
// that it is the same command and not a new one.
func describe(home, location, goos string) Copy {
	if goos == "windows" {
		// curl.exe ships with Windows and runs the same from PowerShell and
		// from cmd. The folder exists, because the copy is in it.
		full := filepath.Join(home, filepath.FromSlash(location))
		return Copy{Path: full, ReplaceCommand: `curl.exe -fsSL ` + SourceURL + ` -o "` + full + `"`}
	}
	saved := "~/" + location
	return Copy{
		Path:           saved,
		ReplaceCommand: "mkdir -p " + path.Dir(saved) + " && curl -fsSL " + SourceURL + " -o " + saved,
	}
}

// readHead returns the first headLimit bytes of a regular file. It asks what
// the name is before it opens it: opening a named pipe would wait for a
// writer, and a version check must never wait.
func readHead(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(name) // #nosec G304 -- a fixed location under the user's home directory, read only
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, headLimit))
}

// Version returns the version a skill file carries, or 0 when it carries
// none. The version is a whole number in the frontmatter's metadata map:
//
//	---
//	name: qurl
//	description: ...
//	metadata:
//	  version: "1"
//	---
//
// The frontmatter is read line by line and not with a YAML parser: the only
// thing wanted is this one line, and a strict parser refuses a description
// that a skill loader accepts.
func Version(head []byte) int {
	text := strings.ReplaceAll(string(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if lines[0] != "---" {
		return 0
	}
	inMetadata := false
	for _, line := range lines[1:] {
		if line == "---" {
			break
		}
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			inMetadata = strings.TrimRight(line, " \t") == "metadata:"
			continue
		}
		if !inMetadata {
			continue
		}
		value, isVersion := strings.CutPrefix(strings.TrimSpace(line), "version:")
		if !isVersion {
			continue
		}
		return wholeNumber(strings.TrimSpace(value))
	}
	return 0
}

// wholeNumber reads a positive whole number, bare or in one pair of quotes.
// Anything else is 0: no sign, no fraction, no word.
func wholeNumber(value string) int {
	for _, quote := range []string{`"`, `'`} {
		if len(value) >= 2 && strings.HasPrefix(value, quote) && strings.HasSuffix(value, quote) {
			value = value[1 : len(value)-1]
			break
		}
	}
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return number
}
