package agent

import "testing"

func TestResourceSDKOriginRemovesOnlyVersionSuffix(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"https://api.layerv.ai/v1":      "https://api.layerv.ai",
		"https://example.test/base/v1/": "https://example.test/base",
		"http://127.0.0.1:8080/v1":      "http://127.0.0.1:8080",
		"https://example.test/base":     "https://example.test/base",
	} {
		got, err := ResourceSDKOrigin(input)
		if err != nil || got != want {
			t.Errorf("ResourceSDKOrigin(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"api.layerv.ai/v1",
		"/v1",
		"ftp://example.test/v1",
		"https://",
		"https://user@example.test/v1",
		"https://example.test/v1?q=x",
		"https://example.test/v1?",
		"https://example.test/v1#x",
		"https://example.test/v1#",
	} {
		if _, err := ResourceSDKOrigin(input); err == nil {
			t.Errorf("ResourceSDKOrigin(%q) accepted unsafe URL", input)
		}
	}
}
