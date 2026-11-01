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
func anonymousPublicDeployment(publisher *qurl.Deployment, cell qurl.DeploymentCell, link qurl.DeploymentCRIDLink) (qurl.Deployment, error) {
	source := *publisher
	cellKey, relayURL := cell.ServerPublicKeyB64, link.RelayURL
	key, err := base64.StdEncoding.DecodeString(cellKey)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != cellKey {
		return qurl.Deployment{}, errors.New("sandbox cell public key is missing or malformed")
	}
	relay, err := url.Parse(relayURL)
	if err != nil || relay.Scheme != "https" || relay.User != nil || relay.Path != "" || relay.RawPath != "" || relay.RawQuery != "" || relay.Fragment != "" || relay.ForceQuery || !strings.HasSuffix(relay.Hostname(), ".layerv.xyz") || !slices.Contains(source.RelayAllowlist, relay.Host) {
		return qurl.Deployment{}, errors.New("sandbox CRID relay does not match the publisher settings")
	}
	cells, err := qurl.NewCellCatalog([]qurl.CellEntry{{CellID: cell.CellID, Host: cell.Host, Port: cell.Port, ServerPublicKeyB64: cellKey}})
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
	endpoint := &qurl.CRIDLinkConfig{RelayURL: relayURL, LinkOrigin: link.LinkOrigin}
	if qurl.CheckCRIDLinkConfigWith(qurl.Config{TrustStore: trust, Cells: cells, RelayAllowlist: qurl.NewRelayAllowlist(source.RelayAllowlist), CRIDLink: endpoint}) != nil {
		return qurl.Deployment{}, errors.New("sandbox CRID endpoint settings are invalid")
	}
	source.Cells = []qurl.DeploymentCell{cell}
	source.CRIDLink = &link
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
	source := qurl.Deployment{Issuers: []qurl.ManifestIssuer{{Kid: "fixture", SPKIDERB64: base64.RawURLEncoding.EncodeToString(der)}}, Cells: []qurl.DeploymentCell{}, RelayAllowlist: []string{"relay.qurl.link.example.layerv.xyz"}}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cell := qurl.DeploymentCell{CellID: "cell0", Host: "cell0.example.layerv.ai", Port: 443, ServerPublicKeyB64: key}
	link := qurl.DeploymentCRIDLink{RelayURL: "https://relay.qurl.link.example.layerv.xyz", LinkOrigin: "https://qurl.link.example.layerv.xyz"}
	got, err := anonymousPublicDeployment(&source, cell, link)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != 1 || !reflect.DeepEqual(got.Cells[0], cell) || got.CRIDLink == nil || got.CRIDLink.LinkOrigin != "https://qurl.link.example.layerv.xyz" || !reflect.DeepEqual(got.Issuers, source.Issuers) || !reflect.DeepEqual(got.RelayAllowlist, source.RelayAllowlist) {
		t.Fatal("anonymous deployment lost its pinned trust or CRID endpoint")
	}
	if source.CRIDLink != nil || len(source.Cells) != 0 {
		t.Fatal("publisher deployment was changed")
	}
	for _, bad := range []struct{ key, relay string }{{"", "https://relay.qurl.link.example.layerv.xyz"}, {base64.StdEncoding.EncodeToString(make([]byte, 31)), "https://relay.qurl.link.example.layerv.xyz"}, {key, "https://relay.layerv.ai"}, {key, "https://other.example.layerv.xyz"}, {key, "http://relay.qurl.link.example.layerv.xyz"}, {key, "https://relay.qurl.link.example.layerv.xyz/path"}} {
		badCell, badLink := cell, link
		badCell.ServerPublicKeyB64, badLink.RelayURL = bad.key, bad.relay
		if _, err := anonymousPublicDeployment(&source, badCell, badLink); err == nil {
			t.Fatal("incomplete or foreign anonymous deployment accepted")
		}
	}
	for _, host := range []string{"", "   "} {
		badCell := cell
		badCell.Host = host
		if _, err := anonymousPublicDeployment(&source, badCell, link); err == nil {
			t.Fatal("missing cell host accepted")
		}
	}
	for _, origin := range []string{"", "http://qurl.link.example.layerv.xyz", "https://qurl.link.example.layerv.xyz/path"} {
		badLink := link
		badLink.LinkOrigin = origin
		if _, err := anonymousPublicDeployment(&source, cell, badLink); err == nil {
			t.Fatal("invalid link origin accepted")
		}
	}
}
