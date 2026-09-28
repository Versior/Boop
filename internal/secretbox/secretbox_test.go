package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// testBinding is the stable context one stored secret is sealed with; the
// settings package passes the setting key here.
const testBinding = "settings.test.key"

func testKey(t *testing.T, fill byte) []byte {
	t.Helper()
	key := make([]byte, KeyBytes)
	for i := range key {
		key[i] = fill
	}
	return key
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	box, err := New(testKey(t, 7))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	values := []string{"", "gho_0123456789", "sk-proj-abcDEF_-123", "空格与中文也要能回来"}
	for _, value := range values {
		nonce, ciphertext, err := box.Seal(testBinding, value)
		if err != nil {
			t.Fatalf("Seal(%q): %v", value, err)
		}
		if len(nonce) != NonceBytes {
			t.Errorf("nonce = %d bytes, want %d", len(nonce), NonceBytes)
		}
		if value != "" && strings.Contains(string(ciphertext), value) {
			t.Errorf("ciphertext of %q contains the plaintext", value)
		}
		opened, err := box.Open(testBinding, nonce, ciphertext)
		if err != nil {
			t.Fatalf("Open(%q): %v", value, err)
		}
		if opened != value {
			t.Errorf("Open = %q, want %q", opened, value)
		}
	}
}

func TestSealUsesAFreshNonce(t *testing.T) {
	box, err := New(testKey(t, 3))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	firstNonce, firstCipher, err := box.Seal(testBinding, "same value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	secondNonce, secondCipher, err := box.Seal(testBinding, "same value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(firstNonce, secondNonce) {
		t.Error("two seals reused the same nonce")
	}
	if bytes.Equal(firstCipher, secondCipher) {
		t.Error("the same plaintext produced identical ciphertext")
	}
}

// TestOpenRejectsAnotherBinding is the documented binding rule: a ciphertext is
// only valid for the exact binding it was sealed with, so moving it to another
// setting (or another row) fails instead of returning plaintext.
func TestOpenRejectsAnotherBinding(t *testing.T) {
	box, err := New(testKey(t, 4))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	nonce, ciphertext, err := box.Seal("github.client_secret", "a-stored-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := box.Open("github.client_id", nonce, ciphertext); !errors.Is(err, ErrCiphertext) {
		t.Errorf("Open with another binding: error = %v, want ErrCiphertext", err)
	}
	if _, err := box.Open("", nonce, ciphertext); !errors.Is(err, ErrCiphertext) {
		t.Errorf("Open without the binding: error = %v, want ErrCiphertext", err)
	}
	if _, err := box.Open("github.client_secret", nonce, ciphertext); err != nil {
		t.Errorf("Open with the original binding: %v", err)
	}
	// The same plaintext under two bindings never produces the same ciphertext.
	otherNonce, otherCipher, err := box.Seal("ai.api_key", "a-stored-secret")
	if err != nil {
		t.Fatalf("second Seal: %v", err)
	}
	if bytes.Equal(nonce, otherNonce) || bytes.Equal(ciphertext, otherCipher) {
		t.Error("two bindings produced identical output")
	}
}

func TestOpenRejectsAWrongKey(t *testing.T) {
	sealer, err := New(testKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	nonce, ciphertext, err := sealer.Seal(testBinding, "github client secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := New(testKey(t, 2))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Open(testBinding, nonce, ciphertext); !errors.Is(err, ErrCiphertext) {
		t.Fatalf("Open with another key: error = %v, want ErrCiphertext", err)
	}
}

func TestOpenRejectsTamperedInput(t *testing.T) {
	box, err := New(testKey(t, 9))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	nonce, ciphertext, err := box.Seal(testBinding, "ai api key")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xff
	if _, err := box.Open(testBinding, nonce, tampered); !errors.Is(err, ErrCiphertext) {
		t.Errorf("tampered ciphertext: error = %v, want ErrCiphertext", err)
	}

	badNonce := append([]byte(nil), nonce...)
	badNonce[0] ^= 0xff
	if _, err := box.Open(testBinding, badNonce, ciphertext); !errors.Is(err, ErrCiphertext) {
		t.Errorf("tampered nonce: error = %v, want ErrCiphertext", err)
	}

	if _, err := box.Open(testBinding, nil, ciphertext); !errors.Is(err, ErrCiphertext) {
		t.Errorf("missing nonce: error = %v, want ErrCiphertext", err)
	}
	if _, err := box.Open(testBinding, nonce, nil); !errors.Is(err, ErrCiphertext) {
		t.Errorf("empty ciphertext: error = %v, want ErrCiphertext", err)
	}
}

func TestNewRejectsBadKeySizes(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, size)); !errors.Is(err, ErrKeySize) {
			t.Errorf("New(%d bytes): error = %v, want ErrKeySize", size, err)
		}
	}
	if _, err := NewFromBase64("not base64!!!"); !errors.Is(err, ErrKeyEncoding) {
		t.Errorf("NewFromBase64(garbage): error = %v, want ErrKeyEncoding", err)
	}
	if _, err := NewFromBase64(base64.StdEncoding.EncodeToString(make([]byte, 16))); !errors.Is(err, ErrKeySize) {
		t.Errorf("NewFromBase64(16 bytes): error = %v, want ErrKeySize", err)
	}
}

func TestNewFromBase64MatchesTheRawKey(t *testing.T) {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := NewFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewFromBase64: %v", err)
	}
	nonce, ciphertext, err := box.Seal(testBinding, "configured")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opened, err := raw.Open(testBinding, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != "configured" {
		t.Errorf("Open = %q, want %q", opened, "configured")
	}
}

func TestNilBoxIsRefused(t *testing.T) {
	var box *Box
	if _, _, err := box.Seal(testBinding, "value"); err == nil {
		t.Error("Seal on a nil box must fail")
	}
	if _, err := box.Open(testBinding, make([]byte, NonceBytes), []byte("x")); err == nil {
		t.Error("Open on a nil box must fail")
	}
}
