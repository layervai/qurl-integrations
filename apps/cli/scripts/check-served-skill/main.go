// Command check-served-skill fails when the qURL agent skill served at
// agentskill.SourceURL is older than agentskill.MinVersion.
//
// The CLI tells a person whose saved copy of the skill is too old to download
// the served one. If the served one is too old as well, that download changes
// nothing and the note stays for good. So the skill has to be served before
// the release that needs it, and this is what holds a release to that order:
// the release workflow runs it on the tagged source before it builds
// anything.
//
// It reads the served file the way the CLI reads a saved copy, so the two
// cannot disagree about what version a file carries. It is the only code in
// this tree that fetches the skill: the CLI itself never does.
//
// With -bundled it checks something else, and not the version: that a copy
// of the skill kept in this tree, such as the one the Claude Code plugin
// ships, is the served file byte for byte. That copy is installed as it is,
// so a copy that differs is a skill no one serves.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/agentskill"
)

const (
	// attempts and pause are for a fetch that may go better a moment later:
	// no answer, a cut-off answer, or a server that says it is in trouble. A
	// served file that is too old is not tried again, and neither is an
	// answer such as 404: waiting does not change either.
	attempts = 3
	pause    = 5 * time.Second

	fetchTimeout = 30 * time.Second

	// userAgent names these requests in the server's log, and keeps them
	// from being turned away as an unnamed program's.
	userAgent = "qurl-served-skill-check/1"

	// fileLimit bounds how much of the served file -bundled reads: many
	// times the skill's size, so a file this long is not a skill.
	fileLimit = 1 << 20
)

// errRefused and errTooLong are answers that the next attempt would get
// again.
var (
	errRefused = errors.New("the server will not serve the file")
	errTooLong = fmt.Errorf("the served file is longer than %d bytes, too long to be the skill", fileLimit)
)

func main() {
	bundled := flag.String("bundled", "", "compare this copy of the skill with the served file, byte for byte, instead of checking the served version")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}
	client := &http.Client{Timeout: fetchTimeout}
	if *bundled != "" {
		if err := compare(context.Background(), client, agentskill.SourceURL, *bundled, pause); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s is the skill served at %s, byte for byte\n", *bundled, agentskill.SourceURL)
		return
	}
	if err := check(context.Background(), client, agentskill.SourceURL, agentskill.MinVersion, pause); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("the skill served at %s is at version %d or later\n", agentskill.SourceURL, agentskill.MinVersion)
}

// check returns an error when the skill served at url carries a version
// below minimum, or none, or cannot be fetched. A fetch that may go better
// a moment later is tried a few times.
func check(ctx context.Context, client *http.Client, url string, minimum int, wait time.Duration) error {
	served, err := fetch(ctx, client, url, wait, agentskill.ReadVersion)
	if err != nil {
		return err
	}
	if served >= minimum {
		return nil
	}
	found := fmt.Sprintf("version %d", served)
	if served == 0 {
		found = "no version the CLI can read"
	}
	return fmt.Errorf("the skill served at %s carries %s, and this CLI needs version %d or later: "+
		"a person who follows the CLI's note would download a copy that is still too old, and the note would never clear. "+
		"Serve the skill with `metadata.version` of at least %d there first, then run this again",
		url, found, minimum, minimum)
}

// compare returns an error when the file at path is not, byte for byte,
// the skill served at url, or when either cannot be read. The local file is
// read first, so a wrong path fails without a fetch.
func compare(ctx context.Context, client *http.Client, url, path string, wait time.Duration) error {
	local, err := os.ReadFile(path) // #nosec G304 -- a path in this tree, named by the workflow that runs this
	if err != nil {
		return fmt.Errorf("could not read the bundled copy of the skill: %w", err)
	}
	served, err := fetch(ctx, client, url, wait, readFile)
	if err != nil {
		return err
	}
	if bytes.Equal(local, served) {
		return nil
	}
	return fmt.Errorf("%s differs from the skill served at %s (%d bytes here, %d served; first difference at byte %d). "+
		"Copy the served file over it, byte for byte, and raise the plugin's version: "+
		"curl -fsSL %s -o %s",
		path, url, len(local), len(served), firstDifference(local, served), url, path)
}

// readFile reads a whole served file, refusing one longer than fileLimit
// rather than comparing the part of it that was read.
func readFile(r io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(r, fileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(content) > fileLimit {
		return nil, errTooLong
	}
	return content, nil
}

// firstDifference returns the offset of the first byte where a and b
// differ; when one is the start of the other, it is the shorter's length.
func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// fetch reads the file served at url with read. A fetch that may go better
// a moment later is tried a few times; an answer that the next attempt
// would get again is not.
func fetch[T any](ctx context.Context, client *http.Client, url string, wait time.Duration, read func(io.Reader) (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		result, err := fetchOnce(ctx, client, url, read)
		if err == nil {
			return result, nil
		}
		if errors.Is(err, errRefused) || errors.Is(err, errTooLong) {
			return result, fmt.Errorf("could not read the skill served at %s: %w", url, err)
		}
		if attempt == attempts {
			return result, fmt.Errorf("could not read the skill served at %s in %d attempts: %w", url, attempts, err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		if ctx.Err() != nil {
			return result, fmt.Errorf("stopped reading the skill served at %s: %w", url, ctx.Err())
		}
	}
}

// fetchOnce fetches url once and reads the file it serves with read.
// Any answer but 200 is an error, as it is for the `curl -f` in the CLI's
// note: an error page has no version, and must not read as an old skill.
// It asks a cache on the way to check that its copy is still the current
// one. A cache may not heed that, and then this reads the copy a person's
// download gets, which is the one that counts.
func fetchOnce[T any](ctx context.Context, client *http.Client, url string, read func(io.Reader) (T, error)) (T, error) {
	var none T
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return none, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return none, err
	}
	defer func() { _ = response.Body.Close() }()
	switch code := response.StatusCode; {
	case code == http.StatusOK:
	case code >= 500, code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return none, fmt.Errorf("the server answered %s", response.Status)
	default:
		return none, fmt.Errorf("%w: it answered %s", errRefused, response.Status)
	}
	return read(response.Body)
}
