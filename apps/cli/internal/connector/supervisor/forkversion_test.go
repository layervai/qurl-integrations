package supervisor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// moduleGoModPath is the repo-root go.mod from this package's directory. A
// constant rather than a walk up the tree: gosec rejects ReadFile on a
// computed path, and readme_notice_test.go already reaches a sibling file
// this way. If the package moves, this fails loudly at the read.
const moduleGoModPath = "../../../../../go.mod"

// forkReplaceDirective pulls the EFFECTIVE fork out of go.mod. The require
// line names the upstream version and is not what compiles; the replace
// target is, so both the path and the version come from here.
var forkReplaceDirective = regexp.MustCompile(`github\.com/fatedier/frp\s+=>\s+(\S+)\s+(v[0-9][0-9A-Za-z.\-+]*)`)

// commentWrap collapses a doc-comment line continuation to a single space, so
// a marker naming the module and version across a wrap still matches. Without
// it the guard silently stops counting a marker the moment someone reflows
// the paragraph, which is the one failure a freshness check must not have.
var commentWrap = regexp.MustCompile(`\n\s*//\s*`)

// TestForkContractMarkersNameThePinnedForkVersion is the freshness guard on
// this package's TODO(upstream-contract) markers.
//
// Those markers each name a module version — the whole point being that a
// reader knows which fork the quoted expressions were verified against. But
// the version is prose: nothing couples it to go.mod, and the edit that most
// needs the markers re-read is exactly the one with no automation on it.
// Dependabot proposes bumps to the require line and does not rewrite replace
// targets, so bumping the fork is a hand edit, and a hand edit that leaves
// four comments confidently citing a version that is no longer compiled.
//
// This does NOT check that the claims are still true — nothing local can, and
// that is why the markers exist. It checks the cheaper half: that the version
// they cite is the version in the build. A bump then fails here, and the
// failure is the prompt to re-verify each claim against the new fork before
// updating the string.
func TestForkContractMarkersNameThePinnedForkVersion(t *testing.T) {
	t.Parallel()

	goMod, err := os.ReadFile(moduleGoModPath)
	if err != nil {
		t.Fatalf("read the module go.mod: %v", err)
	}
	directive := forkReplaceDirective.FindSubmatch(goMod)
	if directive == nil {
		t.Fatal("go.mod has no `replace github.com/fatedier/frp => … v…` directive; if the fork was dropped, drop this guard and the markers' version strings with it")
	}
	pinnedPath, pinnedVersion := string(directive[1]), string(directive[2])
	named := regexp.MustCompile(regexp.QuoteMeta(pinnedPath) + `\s+(v[0-9][0-9A-Za-z.\-+]*)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	sites := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range named.FindAllSubmatch(commentWrap.ReplaceAll(source, []byte(" ")), -1) {
			sites++
			if got := string(match[1]); got != pinnedVersion {
				t.Errorf("%s names %s %s, but go.mod pins %s; re-verify that marker's quoted expressions against the new fork, then update the version it names",
					entry.Name(), pinnedPath, got, pinnedVersion)
			}
		}
	}
	if sites == 0 {
		t.Fatalf("nothing in this package names %s, so the markers this guards are gone or reworded; that is either a regression or a reason to delete this test", pinnedPath)
	}
}
