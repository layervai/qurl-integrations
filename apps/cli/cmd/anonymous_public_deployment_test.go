package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

// The anonymous probe needs a CRID endpoint; the publisher only needs fragment access.
func anonymousPublicDeployment(publisher *qurl.Deployment, cellKey, relayURL string) (qurl.Deployment, error) {
	source := *publisher
	key, err := base64.StdEncoding.DecodeString(cellKey)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != cellKey {
		return qurl.Deployment{}, errors.New("sandbox cell public key is missing or malformed")
	}
	relay, err := url.Parse(relayURL)
	if err != nil || relay.Scheme != "https" || relay.User != nil || relay.Path != "" || relay.RawPath != "" || relay.RawQuery != "" || relay.Fragment != "" || relay.ForceQuery || !strings.HasSuffix(relay.Hostname(), ".layerv.xyz") || !slices.Contains(source.RelayAllowlist, relay.Host) {
		return qurl.Deployment{}, errors.New("sandbox CRID relay does not match the publisher settings")
	}
	cells, err := qurl.NewCellCatalog([]qurl.CellEntry{{CellID: "cell0", Host: "cell0.nhp.layerv.xyz", Port: 443, ServerPublicKeyB64: cellKey}})
	if err != nil {
		return qurl.Deployment{}, errors.New("sandbox CRID cell settings are invalid")
	}
	issuerKeys := make(map[string][]byte, len(source.Issuers))
	for _, issuer := range source.Issuers {
		der, err := base64.RawURLEncoding.DecodeString(issuer.SPKIDERB64)
		if err != nil {
			return qurl.Deployment{}, errors.New("publisher issuer settings are invalid")
		}
		issuerKeys[issuer.Kid] = der
	}
	trust, err := qurl.NewTrustStoreFromDER(issuerKeys)
	if err != nil {
		return qurl.Deployment{}, errors.New("publisher issuer settings are invalid")
	}
	endpoint := &qurl.CRIDLinkConfig{RelayURL: relayURL, LinkOrigin: "https://qurl.link.layerv.xyz"}
	if qurl.CheckCRIDLinkConfigWith(qurl.Config{TrustStore: trust, Cells: cells, RelayAllowlist: qurl.NewRelayAllowlist(source.RelayAllowlist), CRIDLink: endpoint}) != nil {
		return qurl.Deployment{}, errors.New("sandbox CRID endpoint settings are invalid")
	}
	source.Cells = []qurl.DeploymentCell{{CellID: "cell0", Host: "cell0.nhp.layerv.xyz", Port: 443, ServerPublicKeyB64: cellKey}}
	source.CRIDLink = &qurl.DeploymentCRIDLink{RelayURL: relayURL, LinkOrigin: endpoint.LinkOrigin}
	return source, nil
}

func TestAnonymousPublicDeploymentConfiguresLookupWithoutChangingPublisher(t *testing.T) {
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&issuer.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	source := qurl.Deployment{Issuers: []qurl.ManifestIssuer{{Kid: "fixture", SPKIDERB64: base64.RawURLEncoding.EncodeToString(der)}}, Cells: []qurl.DeploymentCell{}, RelayAllowlist: []string{"relay.qurl.link.layerv.xyz"}}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	got, err := anonymousPublicDeployment(&source, key, "https://relay.qurl.link.layerv.xyz")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != 1 || got.Cells[0].ServerPublicKeyB64 != key || got.CRIDLink == nil || got.CRIDLink.LinkOrigin != "https://qurl.link.layerv.xyz" || !reflect.DeepEqual(got.Issuers, source.Issuers) || !reflect.DeepEqual(got.RelayAllowlist, source.RelayAllowlist) {
		t.Fatal("anonymous deployment lost its pinned trust or CRID endpoint")
	}
	if source.CRIDLink != nil || len(source.Cells) != 0 {
		t.Fatal("publisher deployment was changed")
	}
	for _, bad := range []struct{ key, relay string }{{"", "https://relay.qurl.link.layerv.xyz"}, {base64.StdEncoding.EncodeToString(make([]byte, 31)), "https://relay.qurl.link.layerv.xyz"}, {key, "https://relay.layerv.ai"}, {key, "https://other.layerv.xyz"}, {key, "http://relay.qurl.link.layerv.xyz"}, {key, "https://relay.qurl.link.layerv.xyz/path"}} {
		if _, err := anonymousPublicDeployment(&source, bad.key, bad.relay); err == nil {
			t.Fatal("incomplete or foreign anonymous deployment accepted")
		}
	}
}
