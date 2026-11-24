// Package secure holds the backend's cryptography: AES-256-GCM sealing of
// provider keys, argon2id password hashing, random tokens and key derivation.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

// Box seals small secrets with AES-256-GCM. Every Seal uses a fresh random
// 96-bit nonce stored in front of the ciphertext. The additional data binds
// a ciphertext to its owner, so a sealed value copied to another row does
// not open.
type Box struct{ aead cipher.AEAD }

// DeriveKey derives a 32-byte subkey of the master key for one purpose.
func DeriveKey(master []byte, purpose string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte("aihub/"+purpose)), out); err != nil {
		panic(err) // cannot happen for 32 bytes
	}
	return out
}

// NewBox makes a Box from the master key (a subkey is derived for sealing).
func NewBox(master []byte) (*Box, error) {
	if len(master) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(DeriveKey(master, "seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; aad must be given again to Open.
func (b *Box) Seal(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, []byte(aad)), nil
}

// Open decrypts what Seal produced.
func (b *Box) Open(sealed []byte, aad string) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, errors.New("sealed value too short")
	}
	pt, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(aad))
	if err != nil {
		return nil, errors.New("cannot decrypt sealed value")
	}
	return pt, nil
}

// SealString seals a text secret.
func (b *Box) SealString(s, aad string) ([]byte, error) { return b.Seal([]byte(s), aad) }

// OpenString opens a text secret.
func (b *Box) OpenString(sealed []byte, aad string) (string, error) {
	pt, err := b.Open(sealed, aad)
	return string(pt), err
}

// --- passwords (argon2id) ---

type argonParams struct {
	time, memory uint32
	threads      uint8
}

// passwordParams are OWASP's argon2id recommendation (m=19 MiB, t=2, p=1):
// the server is a 1 GB VPS, and every sign-in holds this much memory while
// it hashes. Hashes carry their own parameters, so raising these later
// keeps old hashes valid.
var passwordParams = argonParams{time: 2, memory: 19 * 1024, threads: 1}

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := passwordParams
	key := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash made by HashPassword.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return false
	}
	if p.memory > 1<<20 || p.time > 16 || p.threads == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var dummyHash, _ = HashPassword("aihub-dummy-password")

// DummyPasswordCheck spends as long as a real check, so a login for an
// unknown email is not faster than one with a wrong password.
func DummyPasswordCheck(pw string) { VerifyPassword(pw, dummyHash) }

// --- tokens ---

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RandomString returns n random base62 characters.
func RandomString(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(base62)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = base62[v.Int64()]
	}
	return string(out), nil
}

// RandomURLToken returns base64url of n random bytes.
func RandomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the SHA-256 hex of a token, for tokens we only need to
// recognise (refresh tokens, API keys, invite codes, OAuth states).
func HashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Last4 is what we show of a provider key.
func Last4(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[len(s)-4:]
}
