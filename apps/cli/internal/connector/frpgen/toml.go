package frpgen

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EncodeTOML renders the configuration as an FRP v1 client TOML document. The
// output is deterministic (sorted table keys) so it can be diffed and pinned
// in tests, and every rendered string is validated fail-closed: a value that
// cannot be spelled as a single-line TOML basic string (control characters,
// invalid UTF-8) is rejected rather than escaped into surprise.
//
// The per-cycle knock token is never rendered: MetaQURLKnockToken in the
// common metadata is rejected outright so no caller can turn short-lived
// admission evidence into an on-disk credential.
func EncodeTOML(cfg *ClientConfig) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("nil client config")
	}
	if _, leaked := cfg.Common.Metadatas[MetaQURLKnockToken]; leaked {
		return nil, fmt.Errorf("refusing to render %s into a config file; the supervisor stamps it per cycle", MetaQURLKnockToken)
	}
	var b strings.Builder
	if err := encodeCommonTOML(&b, &cfg.Common); err != nil {
		return nil, err
	}
	if err := encodeProxyTOML(&b, &cfg.Proxy); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func encodeCommonTOML(b *strings.Builder, common *CommonConfig) error {
	if common.ServerAddr != "" {
		if err := writeString(b, "serverAddr", common.ServerAddr); err != nil {
			return err
		}
	}
	if common.ServerPort != 0 {
		writeInt(b, "serverPort", common.ServerPort)
	}
	fmt.Fprintf(b, "loginFailExit = %v\n", common.LoginFailExit)
	if common.Protocol != "" {
		if err := writeString(b, "transport.protocol", common.Protocol); err != nil {
			return err
		}
	}
	writeInt(b, "transport.dialServerTimeout", common.DialServerTimeoutSeconds)
	writeInt(b, "transport.dialServerKeepalive", common.DialServerKeepaliveSeconds)

	if len(common.Metadatas) > 0 {
		b.WriteString("\n[metadatas]\n")
		if err := writeSortedTable(b, common.Metadatas); err != nil {
			return err
		}
	}
	return nil
}

func encodeProxyTOML(b *strings.Builder, proxy *HTTPProxyConfig) error {
	b.WriteString("\n[[proxies]]\n")
	for _, field := range []struct{ key, value string }{
		{"name", proxy.Name},
		{"type", proxy.Type},
		{"localIP", proxy.LocalIP},
	} {
		if err := writeString(b, field.key, field.value); err != nil {
			return err
		}
	}
	writeInt(b, "localPort", proxy.LocalPort)
	if err := writeString(b, "subdomain", proxy.SubDomain); err != nil {
		return err
	}
	if proxy.HostHeaderRewrite != "" {
		if err := writeString(b, "hostHeaderRewrite", proxy.HostHeaderRewrite); err != nil {
			return err
		}
	}
	if proxy.Group != "" {
		if err := writeString(b, "loadBalancer.group", proxy.Group); err != nil {
			return err
		}
		if err := writeString(b, "loadBalancer.groupKey", proxy.GroupKey); err != nil {
			return err
		}
	}
	if len(proxy.Metadatas) > 0 {
		b.WriteString("\n[proxies.metadatas]\n")
		if err := writeSortedTable(b, proxy.Metadatas); err != nil {
			return err
		}
	}
	if len(proxy.RequestHeadersSet) > 0 {
		b.WriteString("\n[proxies.requestHeaders.set]\n")
		if err := writeSortedTable(b, proxy.RequestHeadersSet); err != nil {
			return err
		}
	}
	return nil
}

func writeInt(b *strings.Builder, key string, value int) {
	fmt.Fprintf(b, "%s = %d\n", key, value)
}

func writeString(b *strings.Builder, key, value string) error {
	quoted, err := tomlString(value)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	fmt.Fprintf(b, "%s = %s\n", key, quoted)
	return nil
}

func writeSortedTable(b *strings.Builder, table map[string]string) error {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		quotedKey, err := tomlKey(key)
		if err != nil {
			return err
		}
		quotedValue, err := tomlString(table[key])
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		fmt.Fprintf(b, "%s = %s\n", quotedKey, quotedValue)
	}
	return nil
}

// tomlKey spells a table key: bare where TOML allows it, quoted otherwise.
func tomlKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty table key")
	}
	bare := true
	for _, r := range key {
		isBare := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !isBare {
			bare = false
			break
		}
	}
	if bare {
		return key, nil
	}
	return tomlString(key)
}

// tomlString spells a value as a single-line TOML basic string. Only quote
// and backslash need escaping once control characters and invalid UTF-8 are
// rejected — and they are rejected deliberately: none of the identities,
// hosts, or header values this generator renders legitimately carry them, so
// an occurrence is a bug or an injection attempt, not data to preserve.
func tomlString(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("value %q is not valid UTF-8", value)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("value %s contains control characters", strconv.Quote(value))
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`, nil
}
