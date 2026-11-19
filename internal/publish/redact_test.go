package publish_test

import (
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/publish"
)

func TestRedactSecretsAndMentions(t *testing.T) {
	const (
		apiKey   = "sk-live-abcdefghijklmnopqrstuvwxyz"
		ghPat    = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
		finePat  = "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz"
		awsKey   = "AKIAIOSFODNN7EXAMPLE"
		certHead = "-----BEGIN RSA PRIVATE KEY-----"
	)
	r := publish.NewRedactor(apiKey)

	input := "Revisión de @juan con " + apiKey + ", " + ghPat + ", " +
		finePat + ", " + awsKey + " y " + certHead
	got, changed := r.Redact(input)
	if !changed {
		t.Fatal("Redact() changed = false, want true when secrets are present")
	}
	for _, secret := range []string{apiKey, ghPat, finePat, awsKey, certHead} {
		if strings.Contains(got, secret) {
			t.Errorf("Redact() left secret %q in %q", secret, got)
		}
	}
	if strings.Count(got, "[redacted]") != 5 {
		t.Errorf("Redact() = %q, want five [redacted] markers", got)
	}
	if want := "`@`juan"; !strings.Contains(got, want) {
		t.Errorf("Redact() = %q, want mention escaped as %q", got, want)
	}

	if got, changed := r.Redact("sin secretos"); changed || got != "sin secretos" {
		t.Errorf("Redact(clean) = %q, %v; want unchanged text and false", got, changed)
	}

	if got := publish.EscapeMentions("hola @ana y @acme/equipo, correo a@b.com"); got != "hola `@`ana y `@`acme/equipo, correo a@b.com" {
		t.Errorf("EscapeMentions() = %q", got)
	}
}
