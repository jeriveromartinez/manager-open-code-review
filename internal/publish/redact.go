package publish

import (
	"regexp"
	"strings"
)

const redactedMarker = "[redacted]"

// Redactor removes known secret values and credential-shaped strings from text
// before it reaches GitHub. Secrets are literal values (an LLM API key or a
// clone token); patterns cover credentials that may appear in LLM output.
type Redactor struct {
	Secrets []string
}

// NewRedactor builds a Redactor from the secrets supplied by the caller. Empty
// values are ignored so an unconfigured secret cannot redact every space.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, secret := range secrets {
		if s := strings.TrimSpace(secret); s != "" {
			r.Secrets = append(r.Secrets, s)
		}
	}
	return r
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*-----`),
}

func (r *Redactor) hasSecret(s string) bool {
	for _, secret := range r.Secrets {
		if strings.Contains(s, secret) {
			return true
		}
	}
	for _, pattern := range secretPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// Redact replaces every known secret and credential pattern with [redacted]
// and escapes @mentions. The boolean reports whether a secret was removed;
// escaping alone is not a redaction alert.
func (r *Redactor) Redact(s string) (string, bool) {
	found := r.hasSecret(s)
	out := s
	if found {
		for _, secret := range r.Secrets {
			if strings.Contains(out, secret) {
				out = strings.ReplaceAll(out, secret, redactedMarker)
			}
		}
		for _, pattern := range secretPatterns {
			out = pattern.ReplaceAllString(out, redactedMarker)
		}
	}
	return EscapeMentions(out), found
}

var mentionPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9][A-Za-z0-9_/-]*)`)

// EscapeMentions prevents a published mention from notifying its owner by
// wrapping the @ in backticks. Emails are left alone.
func EscapeMentions(s string) string {
	return mentionPattern.ReplaceAllString(s, "${1}`@`${2}")
}

// sanitize applies redaction and mention escaping to one published string and
// records whether a secret was found. It never returns secret material.
func (p *publishContext) sanitize(s string) string {
	clean, secretFound := p.redactor.Redact(s)
	if secretFound {
		p.redacted = true
	}
	return clean
}
