// verify-ownership reads a signed link from stdin and emits public identity only.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

func main() {
	config, err := loadPublicConfig(os.Getenv("QURL_PUBLIC_CONFIG_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "public ownership config unavailable")
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "--check-config" {
		if _, err := issuerTrust(config); err != nil {
			fmt.Fprintln(os.Stderr, "invalid issuer trust")
			os.Exit(1)
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	if err != nil || len(raw) > 65536 {
		fmt.Fprintln(os.Stderr, "invalid ownership input")
		os.Exit(1)
	}
	identity, err := verifiedPublicIdentity(strings.TrimSpace(string(raw)), config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ownership verification failed")
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(identity); err != nil {
		os.Exit(1)
	}
}

type publicConfig struct {
	Issuers map[string]string
	CellKey string
}

// TODO(upstream-contract): NHP terraform/modules/qurl-link/frontend/index.html
// declares these exact public literals. Never evaluate HTML or JavaScript.
func parsePublicConfig(html string) (publicConfig, error) {
	var config publicConfig
	blocks := regexp.MustCompile(`(?s)const QURL_LINK_CONFIG = \{(.*?)\n {6}\};`).FindAllStringSubmatch(html, -1)
	if len(blocks) != 1 {
		return config, errors.New("missing or duplicate public config")
	}
	issuers := regexp.MustCompile(`(?m)^ {8}issuerTrustStore: (\{[^\r\n]*\}),$`).FindAllStringSubmatch(blocks[0][1], -1)
	cell := regexp.MustCompile(`(?m)^ {8}serverStaticPubB64: ("[^"\r\n]*"),$`).FindAllStringSubmatch(blocks[0][1], -1)
	if len(issuers) != 1 || len(cell) != 1 {
		return config, errors.New("invalid public config shape")
	}
	if err := json.Unmarshal([]byte(issuers[0][1]), &config.Issuers); err != nil || len(config.Issuers) == 0 {
		return config, errors.New("invalid issuer config")
	}
	if err := json.Unmarshal([]byte(cell[0][1]), &config.CellKey); err != nil {
		return config, errors.New("invalid cell config")
	}
	key, err := base64.StdEncoding.DecodeString(config.CellKey)
	if err != nil || len(key) != 32 {
		return config, errors.New("invalid cell public key")
	}
	return config, nil
}

func loadPublicConfig(endpoint string) (publicConfig, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || (u.Host != "qurl.link.layerv.xyz" && u.Host != "qurl.link") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "/" && u.Path != "") {
		return publicConfig{}, errors.New("invalid public config origin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return publicConfig{}, err
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return publicConfig{}, errors.New("public config request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return publicConfig{}, errors.New("public config status failed")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if err != nil || len(raw) > 1048576 {
		return publicConfig{}, errors.New("invalid public config body")
	}
	return parsePublicConfig(string(raw))
}

func issuerTrust(config publicConfig) (*qurl.TrustStore, error) {
	keys := map[string][]byte{}
	for kid, encoded := range config.Issuers {
		der, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("invalid issuer public key")
		}
		keys[kid] = der
	}
	return qurl.NewTrustStoreFromDER(keys)
}

func verifiedPublicIdentity(link string, config publicConfig) (map[string]string, error) {
	trust, err := issuerTrust(config)
	if err != nil {
		return nil, errors.New("invalid issuer trust")
	}
	frag, err := qurl.VerifyLink(link, trust)
	if err != nil {
		return nil, errors.New("signed ownership verification failed")
	}
	agent, err := base64.RawURLEncoding.DecodeString(frag.Claims.QurlUserPublicKeyB64)
	if err != nil || len(agent) != 32 {
		return nil, errors.New("invalid public agent identity")
	}
	cellKey, err := base64.RawURLEncoding.DecodeString(frag.Claims.CellPublicKeyB64)
	if err != nil || len(cellKey) != 32 {
		return nil, errors.New("invalid public cell identity")
	}
	// TODO(upstream-contract): cell_id is optional in qURL v2; retain it without inventing a value.
	cellID := frag.Claims.CellID
	if config.CellKey != base64.StdEncoding.EncodeToString(cellKey) {
		return nil, errors.New("browser cell identity mismatch")
	}
	return map[string]string{"agent_public_key": base64.StdEncoding.EncodeToString(agent), "resource_public_key_b64": frag.Claims.ResourcePublicKeyB64, "cell_public_key_b64": base64.StdEncoding.EncodeToString(cellKey), "cell_id": cellID, "signed_jti": frag.Claims.Jti, "signed_expiry_unix": strconv.FormatInt(frag.Claims.Exp, 10)}, nil
}
