// Package redact removes secrets from text before it is buffered, returned to
// the browser or written anywhere else.
//
// The motivating case is the upstream frp-panel client, which prints its own
// client secret in some log lines (for example the auto-join path). Every byte
// of child process output goes through a Redactor, so a secret that we know the
// literal value of can never reach the log ring buffer or the web UI.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces every redacted value.
const Placeholder = "***"

type pattern struct {
	re   *regexp.Regexp
	repl string
}

// keyValuePatterns catch `secret=...`, `"token": "..."`, `--secret abc` and
// similar shapes. They deliberately require an explicit assignment or flag
// syntax so that plain sentences such as "client secret cannot be empty" are
// left intact.
var keyValuePatterns = []pattern{
	// --secret=value / --secret value
	{regexp.MustCompile(`(?i)(--?secret[=\s]+)([^\s,;]+)`), "${1}" + Placeholder},
	// -s value / -s=value (the upstream client's short flag for the secret),
	// anchored on a boundary so that an unrelated "-s" inside a word such as
	// "logs" is not treated as a flag.
	{regexp.MustCompile(`(?i)(^|[\s'"(\[,;])(-s[=\s]+)([^\s,;]+)`), "${1}${2}" + Placeholder},
	// key=value, key: value, key = value  ("key" may be quoted)
	{regexp.MustCompile(`(?i)(["']?\b(?:client_?secret|secret|token|join_?token|password|passwd|pwd|api_?key|apikey|access_?key|private_?key)["']?\s*[=:]\s*["']?)([^\s"',;]{3,})`), "${1}" + Placeholder},
	// Authorization: Bearer xyz
	{regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)([A-Za-z0-9._~+/=-]{6,})`), "${1}" + Placeholder},
	// Bare bearer token
	{regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{12,})`), "${1}" + Placeholder},
}

// Redactor is safe for concurrent use: AddLiterals may be called at runtime
// (a configuration change) while child process output is being redacted.
type Redactor struct {
	mu       sync.RWMutex
	literals []string
	extra    []pattern
}

// New builds a Redactor for the given literal secret values.
func New(literals ...string) *Redactor {
	r := &Redactor{}
	r.AddLiterals(literals...)
	return r
}

// AddLiterals registers additional literal values to strip.
func (r *Redactor) AddLiterals(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		// Never skip a known secret: a short secret that leaks is still a
		// leaked secret. Redacting it may mangle unrelated log text, which is
		// an acceptable trade for the guarantee.
		if v == "" {
			continue
		}
		seen := false
		for _, existing := range r.literals {
			if existing == v {
				seen = true
				break
			}
		}
		if !seen {
			r.literals = append(r.literals, v)
		}
	}
	// Longest first so that a secret containing another secret still matches
	// the most specific value.
	sort.Slice(r.literals, func(i, j int) bool { return len(r.literals[i]) > len(r.literals[j]) })
}

// Redact returns text with every known secret replaced by Placeholder.
func (r *Redactor) Redact(text string) string {
	if r == nil || text == "" {
		return text
	}
	// The read lock is held for the whole replacement pass: releasing it after
	// copying the slice header would still let a concurrent AddLiterals mutate
	// the backing array underneath us (append + sort.Slice).
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, lit := range r.literals {
		text = strings.ReplaceAll(text, lit, Placeholder)
	}
	for _, p := range keyValuePatterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	for _, p := range r.extra {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	return text
}

// HasLiterals reports whether any literal secret was registered.
func (r *Redactor) HasLiterals() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.literals) > 0
}
