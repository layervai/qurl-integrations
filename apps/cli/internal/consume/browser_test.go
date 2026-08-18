package consume

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func envOf(m map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// TestBrowserCommand pins the override precedence (QURL_BROWSER, then
// BROWSER, then the platform default) and the argument splitting contract.
func TestBrowserCommand(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		goos string
		want []string
	}{
		{name: "qurl override wins", env: map[string]string{"QURL_BROWSER": "firefox", "BROWSER": "chromium"}, goos: "linux", want: []string{"firefox"}},
		{name: "generic override second", env: map[string]string{"BROWSER": "chromium"}, goos: "linux", want: []string{"chromium"}},
		{name: "override carries arguments", env: map[string]string{"QURL_BROWSER": "firefox --new-tab"}, goos: "darwin", want: []string{"firefox", "--new-tab"}},
		{name: "blank override ignored", env: map[string]string{"QURL_BROWSER": "   "}, goos: "darwin", want: []string{"open"}},
		{name: "darwin default", env: nil, goos: "darwin", want: []string{"open"}},
		{name: "windows default", env: nil, goos: "windows", want: []string{"rundll32", "url.dll,FileProtocolHandler"}},
		{name: "linux default", env: nil, goos: "linux", want: []string{"xdg-open"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BrowserCommand(envOf(tc.env), tc.goos)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("BrowserCommand = %q, want %q", got, tc.want)
			}
		})
	}
	if got := BrowserCommand(nil, "linux"); !slices.Equal(got, []string{"xdg-open"}) {
		t.Fatalf("nil lookup: BrowserCommand = %q, want xdg-open", got)
	}
}

// TestLauncherOpen pins that the link is appended as the final argument and
// that the injected runner is what actually executes.
func TestLauncherOpen(t *testing.T) {
	var got []string
	l := &Launcher{
		LookupEnv: envOf(map[string]string{"QURL_BROWSER": "mybrowser --flag"}),
		GOOS:      "linux",
		Run: func(_ context.Context, argv []string) error {
			got = slices.Clone(argv)
			return nil
		},
	}
	if err := l.Open(context.Background(), "https://qurl.link/#frag"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []string{"mybrowser", "--flag", "https://qurl.link/#frag"}
	if !slices.Equal(got, want) {
		t.Fatalf("ran %q, want %q", got, want)
	}
}

// TestLauncherOpenSurfacesFailure pins that a failed launcher names the
// command it ran and keeps the cause reachable.
func TestLauncherOpenSurfacesFailure(t *testing.T) {
	boom := errors.New("exec format error")
	l := &Launcher{GOOS: "linux", Run: func(context.Context, []string) error { return boom }}
	err := l.Open(context.Background(), "https://example.com")
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Open err = %v, want the runner's failure in the chain", err)
	}
}

// TestOpenRefusesNonWebSchemes pins the launcher's scheme gate: CRID
// verification proves the answer matches the CRID, not that the link is
// browser-safe, so anything but http(s) is refused unlaunched.
func TestOpenRefusesNonWebSchemes(t *testing.T) {
	for _, link := range []string{"javascript:alert(1)", "file:///etc/passwd", "-http://x", "vbscript:x", ""} {
		ran := false
		l := &Launcher{GOOS: "darwin", Run: func(context.Context, []string) error { ran = true; return nil }}
		err := l.Open(context.Background(), link)
		if !errors.Is(err, ErrUnopenableLink) {
			t.Errorf("Open(%q) err = %v, want ErrUnopenableLink", link, err)
		}
		if ran {
			t.Errorf("Open(%q) launched the browser", link)
		}
	}
	ok := false
	l := &Launcher{GOOS: "darwin", Run: func(context.Context, []string) error { ok = true; return nil }}
	if err := l.Open(context.Background(), "https://qurl.link/#qv2.x"); err != nil || !ok {
		t.Errorf("https link must launch: err=%v launched=%t", err, ok)
	}
}
