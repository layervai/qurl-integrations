package consume

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

// siteOpener writes deployment settings whose CRID link entry names origin,
// or no such entry when origin is empty, and returns an opener that reads
// them through its injected environment. The host names are placeholders: no
// test here resolves or contacts them.
func siteOpener(t *testing.T, origin string) *AccessOpener {
	t.Helper()
	signer, err := qurl.GenerateLocalSigner("kid-link-site")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	der, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatalf("public key DER: %v", err)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate cell key: %v", err)
	}
	settings := qurl.Deployment{
		Issuers: []qurl.ManifestIssuer{{Kid: signer.KID(), SPKIDERB64: base64.RawURLEncoding.EncodeToString(der)}},
		Cells: []qurl.DeploymentCell{{
			CellID: "cell-site", Host: "cell-site.example.test", Port: 443,
			ServerPublicKeyB64: base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
		}},
		RelayAllowlist: []string{"requests.example.test"},
	}
	if origin != "" {
		settings.CRIDLink = &qurl.DeploymentCRIDLink{RelayURL: "https://requests.example.test", LinkOrigin: origin}
	}
	raw, err := json.Marshal(&settings)
	if err != nil {
		t.Fatalf("encode deployment fixture: %v", err)
	}
	return &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: writeDeployment(t, string(raw))})}
}

// TestLinkSiteIsOnlyWhatTheSettingsName pins where the link site comes from:
// the origin the deployment settings name, and nothing else. Without settings
// that name one, and for an origin that is not a bare HTTPS origin, the
// answer is "not known". No host is ever made up.
func TestLinkSiteIsOnlyWhatTheSettingsName(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	for name, test := range map[string]struct {
		origin string
		want   string
	}{
		"an origin":           {origin: "https://links.example.test", want: "https://links.example.test"},
		"an origin with port": {origin: "https://links.example.test:8443", want: "https://links.example.test:8443"},
		"no entry":            {},
		"plain http":          {origin: "http://links.example.test"},
		"a path":              {origin: "https://links.example.test/open"},
		"a trailing slash":    {origin: "https://links.example.test/"},
		"a query":             {origin: "https://links.example.test?x=1"},
		"a fragment":          {origin: "https://links.example.test#x"},
		"a user":              {origin: "https://user@links.example.test"},
		"no host":             {origin: "https://"},
		"not a URL":           {origin: "links.example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := siteOpener(t, test.origin).LinkSite(); got != test.want {
				t.Fatalf("LinkSite() = %q, want %q", got, test.want)
			}
		})
	}

	for name, opener := range map[string]*AccessOpener{
		"no environment":   {},
		"no settings file": {LookupEnv: envMap(nil)},
		"a blank path":     {LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: "  "})},
		"a missing file":   {LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: t.TempDir() + "/none.json"})},
		"a malformed file": {LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: writeDeployment(t, "not json")})},
	} {
		if got := opener.LinkSite(); got != "" {
			t.Errorf("%s: LinkSite() = %q, want it not known", name, got)
		}
	}
}

// TestResourceAddress pins the address of a resource on the link site, and
// that there is none without a site or without a CRID.
func TestResourceAddress(t *testing.T) {
	const resourceCRID = "qexamplecrid"
	if got, want := ResourceAddress("https://links.example.test", resourceCRID), "https://links.example.test/"+resourceCRID; got != want {
		t.Fatalf("ResourceAddress = %q, want %q", got, want)
	}
	if got := ResourceAddress("", resourceCRID); got != "" {
		t.Fatalf("an address was made without a link site: %q", got)
	}
	if got := ResourceAddress("https://links.example.test", ""); got != "" {
		t.Fatalf("an address was made without a CRID: %q", got)
	}
}
