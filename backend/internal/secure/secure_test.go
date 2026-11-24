package secure

import (
	"bytes"
	"strings"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestBoxRoundTrip(t *testing.T) {
	b, err := NewBox(testKey())
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := b.SealString("sk-secret", "cred:1:openai")
	s2, _ := b.SealString("sk-secret", "cred:1:openai")
	if bytes.Equal(s1, s2) {
		t.Fatal("two seals of the same value must differ (fresh nonce)")
	}
	if bytes.Contains(s1, []byte("sk-secret")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	got, err := b.OpenString(s1, "cred:1:openai")
	if err != nil || got != "sk-secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := b.OpenString(s1, "cred:2:openai"); err == nil {
		t.Fatal("other AAD must not open")
	}
	s1[len(s1)-1] ^= 1
	if _, err := b.OpenString(s1, "cred:1:openai"); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}
	other, _ := NewBox(bytes.Repeat([]byte{8}, 32))
	if _, err := other.OpenString(s2, "cred:1:openai"); err == nil {
		t.Fatal("other master key must not open")
	}
	if _, err := NewBox([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash format: %s", h)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Fatal("right password rejected")
	}
	if VerifyPassword("wrong", h) || VerifyPassword("correct horse battery", "garbage") {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestTokens(t *testing.T) {
	a, _ := RandomString(40)
	b, _ := RandomString(40)
	if len(a) != 40 || a == b {
		t.Fatal("random string")
	}
	if HashToken("x") != HashToken("x") || len(HashToken("x")) != 64 {
		t.Fatal("hash")
	}
	if Last4("sk-abcdef") != "cdef" || Last4("ab") != "**" {
		t.Fatal("last4")
	}
	if !bytes.Equal(DeriveKey(testKey(), "a"), DeriveKey(testKey(), "a")) || bytes.Equal(DeriveKey(testKey(), "a"), DeriveKey(testKey(), "b")) {
		t.Fatal("derive")
	}
}
