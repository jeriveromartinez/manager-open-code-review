package crypto_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
)

func newTestBox(t *testing.T, fill byte) *crypto.Box {
	t.Helper()

	box, err := crypto.NewBox(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatalf("NewBox() error = %v", err)
	}
	return box
}

func TestRoundTrip(t *testing.T) {
	box := newTestBox(t, 0x01)
	plain := []byte(`{"private_key":"-----BEGIN RSA PRIVATE KEY-----\nMIIE...","client_secret":"s3cr3t"}`)

	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("Seal() = %q, want standard base64: %v", sealed, err)
	}
	// nonce (12) || ciphertext (len(plain)) || tag (16)
	if want := 12 + len(plain) + 16; len(raw) != want {
		t.Errorf("len(sealed) = %d, want %d", len(raw), want)
	}

	got, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("Open() = %q, want %q", got, plain)
	}
}

func TestNonceRandom(t *testing.T) {
	box := newTestBox(t, 0x02)
	plain := []byte("mismo texto, nonce distinto")

	first, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	second, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	if first == second {
		t.Error("Seal() returned identical output twice, want a fresh nonce each call")
	}

	for _, sealed := range []string{first, second} {
		got, err := box.Open(sealed)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("Open() = %q, want %q", got, plain)
		}
	}
}

func TestWrongKey(t *testing.T) {
	sealed, err := newTestBox(t, 0x03).Seal([]byte("secreto"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	got, err := newTestBox(t, 0x04).Open(sealed)
	if err == nil {
		t.Fatal("Open() error = nil, want error with a different key")
	}
	if got != nil {
		t.Errorf("Open() = %q, want nil plaintext", got)
	}
}

func TestTampered(t *testing.T) {
	box := newTestBox(t, 0x05)
	plain := []byte("secreto que no debe filtrarse")

	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("Seal() = %q, want standard base64: %v", sealed, err)
	}

	flipped := bytes.Clone(raw)
	flipped[len(flipped)-1] ^= 0xff

	tests := []struct {
		name    string
		sealed  string
		wantErr bool
	}{
		{name: "flipped ciphertext byte", sealed: base64.StdEncoding.EncodeToString(flipped), wantErr: true},
		{name: "truncated payload", sealed: base64.StdEncoding.EncodeToString(raw[:len(raw)-1]), wantErr: true},
		{name: "malformed base64", sealed: "no es base64!!", wantErr: true},
		{name: "empty", sealed: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := box.Open(tt.sealed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Open() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("Open() = %q, want nil plaintext on error", got)
			}
		})
	}
}

func TestKeyLength(t *testing.T) {
	tests := []struct {
		name string
		key  []byte
	}{
		{name: "nil", key: nil},
		{name: "16 bytes", key: bytes.Repeat([]byte{0x06}, 16)},
		{name: "31 bytes", key: bytes.Repeat([]byte{0x06}, 31)},
		{name: "33 bytes", key: bytes.Repeat([]byte{0x06}, 33)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box, err := crypto.NewBox(tt.key)
			if err == nil {
				t.Fatalf("NewBox() error = nil, want error for %d bytes", len(tt.key))
			}
			if box != nil {
				t.Errorf("NewBox() = %v, want nil box", box)
			}
			if !strings.Contains(err.Error(), "32") {
				t.Errorf("NewBox() error = %q, want it to mention 32 bytes", err.Error())
			}
		})
	}
}
