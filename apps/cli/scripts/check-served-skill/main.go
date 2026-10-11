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
package main

import (
	"context"
	"errors"
	"fmt"
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
	userAgent = "qurl-release-check/1"
)

// errRefused is an answer that the next attempt would get again.
var errRefused = errors.New("the server will not serve the file")

func main() {
	client := &http.Client{Timeout: fetchTimeout}
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
	var served int
	var err error
	for attempt := 1; ; attempt++ {
		served, err = servedVersion(ctx, client, url)
		if err == nil {
			break
		}
		if errors.Is(err, errRefused) {
			return fmt.Errorf("could not read the skill served at %s: %w", url, err)
		}
		if attempt == attempts {
			return fmt.Errorf("could not read the skill served at %s in %d attempts: %w", url, attempts, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if served >= minimum {
		return nil
	}
	found := fmt.Sprintf("version %d", served)
	if served == 0 {
		found = "no version the CLI can read"
	}
	return fmt.Errorf("the skill served at %s carries %s, and this release needs version %d or later: "+
		"a person who follows the CLI's note would download a copy that is still too old, and the note would never clear. "+
		"Serve the skill with `metadata.version` of at least %d there first, then run this again",
		url, found, minimum, minimum)
}

// servedVersion fetches url once and returns the version its file carries.
// Any answer but 200 is an error, as it is for the `curl -f` in the CLI's
// note: an error page has no version, and must not read as an old skill.
// It asks for the file as it is now and not as a cache last saw it, so that
// a run made right after the skill was replaced reads the new one.
func servedVersion(ctx context.Context, client *http.Client, url string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	switch code := response.StatusCode; {
	case code == http.StatusOK:
	case code >= 500, code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return 0, fmt.Errorf("the server answered %s", response.Status)
	default:
		return 0, fmt.Errorf("%w: it answered %s", errRefused, response.Status)
	}
	return agentskill.ReadVersion(response.Body)
}
