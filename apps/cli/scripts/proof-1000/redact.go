package main

import (
	"regexp"
	"strings"
)

// redactor scrubs everything a shared run directory must not carry: owner
// ids, API-key prefixes, bearer tokens, in-link credentials, the Hub trust
// values, and the user's home directory.
type redactor struct {
	literals [][2]string
	patterns []*regexp.Regexp
	repl     []string
}

func newRedactor(home string, literals ...string) *redactor {
	r := &redactor{}
	for i := 0; i+1 < len(literals); i += 2 {
		if strings.TrimSpace(literals[i]) == "" {
			continue
		}
		r.literals = append(r.literals, [2]string{literals[i], literals[i+1]})
	}
	if home != "" && home != "/" {
		r.literals = append(r.literals, [2]string{home, "~"})
	}
	add := func(pattern, replacement string) {
		r.patterns = append(r.patterns, regexp.MustCompile(pattern))
		r.repl = append(r.repl, replacement)
	}
	add(`auth0\|[A-Za-z0-9]+`, "auth0|<redacted>")
	add(`\blv_(live|test)_[A-Za-z0-9]+`, "lv_$1_<redacted>")
	add(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`, "Bearer <redacted>")
	add(`#[A-Za-z0-9._~-]{24,}`, "#<credential>")
	add(`"owner_id"\s*:\s*"[^"]*"`, `"owner_id":"<redacted>"`)
	return r
}

func (r *redactor) apply(s string) string {
	if r == nil {
		return s
	}
	for _, lit := range r.literals {
		s = strings.ReplaceAll(s, lit[0], lit[1])
	}
	for i, p := range r.patterns {
		s = p.ReplaceAllString(s, r.repl[i])
	}
	return s
}
