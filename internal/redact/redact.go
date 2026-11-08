// Package redact masks secrets in text the host keeps or shows: the exact
// values it has handled (keys it wrote, tokens it sent) and the shapes
// well-known keys take, so a plugin that logs a response doesn't keep the key.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Mask replaces a secret; a redacted value always reads the same, so a line
// can be searched for it.
const Mask = "[redacted]"

// minLength keeps short values out of the vault: masking "true" or a port
// would mangle every line.
const minLength = 8

// patterns are keys recognised by shape, each masked whole. The generic one
// keeps the field's name and masks its value.
var patterns = []*regexp.Regexp{
	// Stripe, GitHub, Slack, Anthropic, OpenAI, RepoGo plugin keys, AWS access key ids.
	regexp.MustCompile(`\b(?:sk|rk|whsec)_(?:live|test)?_?[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\brgk_[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	// JWTs and bearer tokens.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
}

// generic is `key: value` in text or JSON, for a field named like a secret.
var generic = regexp.MustCompile(`(?i)(["']?(?:[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|private[_-]?key|authorization|credential)[a-z0-9_-]*)["']?\s*[:=]\s*["']?)([^\s"',}\[\]\\]{8,})`)

// Text masks what looks like a key, whoever's it is.
func Text(s string) string {
	for _, p := range patterns {
		s = p.ReplaceAllString(s, Mask)
	}
	return generic.ReplaceAllString(s, "${1}"+Mask)
}

// Vault is the exact secret values this host has handled, masked wherever
// they appear. Safe from any goroutine.
type Vault struct {
	mu     sync.RWMutex
	values map[string]bool
	// sorted is the values longest first, so a value holding another is masked whole.
	sorted []string
}

func New() *Vault { return &Vault{values: map[string]bool{}} }

// Add remembers values to mask; short or empty ones are ignored.
func (v *Vault) Add(values ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	added := false
	for _, s := range values {
		if len(s) < minLength || v.values[s] {
			continue
		}
		v.values[s] = true
		v.sorted = append(v.sorted, s)
		added = true
	}
	if added {
		sort.Slice(v.sorted, func(i, j int) bool { return len(v.sorted[i]) > len(v.sorted[j]) })
	}
}

// Text masks the vault's values, then what looks like a key.
func (v *Vault) Text(s string) string {
	v.mu.RLock()
	for _, value := range v.sorted {
		if strings.Contains(s, value) {
			s = strings.ReplaceAll(s, value, Mask)
		}
	}
	v.mu.RUnlock()
	return Text(s)
}
