package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	EnvelopeMagic       = "LTC1"
	NonceSize           = 12
	PublicKeyPrefix     = "ltpub_"
	PrivateKeyPrefix    = "ltsec_"
	KeyDerivationInfo   = "liltok-v1-cache-encryption"
	MinEnvelopeSize     = 4 + 32 + NonceSize + 16
)

var (
	DefaultMaintainerPublicKey = ""

	ErrInvalidMagic      = errors.New("invalid envelope magic header")
	ErrPayloadTooShort   = errors.New("envelope payload too short")
	ErrInvalidKeyFormat  = errors.New("invalid key format")
	ErrDecryptionFailed  = errors.New("decryption failed: authenticating tag mismatch or invalid key")
)

// KeyPair holds private and public key representations for maintainers.
type KeyPair struct {
	PrivateKeyStr string
	PublicKeyStr  string
	PrivateKey    *ecdh.PrivateKey
	PublicKey     *ecdh.PublicKey
}

// GenerateKeyPair produces a new X25519 key pair with formatted strings.
func GenerateKeyPair() (*KeyPair, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate X25519 key: %w", err)
	}

	pub := priv.PublicKey()
	privB64 := base64.RawURLEncoding.EncodeToString(priv.Bytes())
	pubB64 := base64.RawURLEncoding.EncodeToString(pub.Bytes())

	return &KeyPair{
		PrivateKeyStr: PrivateKeyPrefix + privB64,
		PublicKeyStr:  PublicKeyPrefix + pubB64,
		PrivateKey:    priv,
		PublicKey:     pub,
	}, nil
}

// ParsePublicKey parses a base64 or prefixed X25519 public key.
func ParsePublicKey(keyStr string) (*ecdh.PublicKey, error) {
	trimmed := strings.TrimSpace(keyStr)
	trimmed = strings.TrimPrefix(trimmed, PublicKeyPrefix)

	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid base64 encoding", ErrInvalidKeyFormat)
		}
	}

	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: public key must be exactly 32 bytes (got %d)", ErrInvalidKeyFormat, len(raw))
	}

	return ecdh.X25519().NewPublicKey(raw)
}

// ParsePrivateKey parses a base64 or prefixed X25519 private key.
func ParsePrivateKey(keyStr string) (*ecdh.PrivateKey, error) {
	trimmed := strings.TrimSpace(keyStr)
	trimmed = strings.TrimPrefix(trimmed, PrivateKeyPrefix)

	raw, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid base64 encoding", ErrInvalidKeyFormat)
		}
	}

	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: private key must be exactly 32 bytes (got %d)", ErrInvalidKeyFormat, len(raw))
	}

	return ecdh.X25519().NewPrivateKey(raw)
}

// EncryptPayload encrypts plaintext using ephemeral X25519 ECDH + AES-256-GCM.
func EncryptPayload(recipientPub *ecdh.PublicKey, plaintext []byte) ([]byte, error) {
	if recipientPub == nil {
		return nil, errors.New("recipient public key is required")
	}

	curve := ecdh.X25519()
	ephemeralPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral key: %w", err)
	}
	ephemeralPub := ephemeralPriv.PublicKey()

	sharedSecret, err := ephemeralPriv.ECDH(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("ecdh key agreement failed: %w", err)
	}

	aesKey := hkdfSHA256(sharedSecret, nil, []byte(KeyDerivationInfo), 32)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm creation failed: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce generation failed: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(EnvelopeMagic))

	var buf bytes.Buffer
	buf.WriteString(EnvelopeMagic)
	buf.Write(ephemeralPub.Bytes())
	buf.Write(nonce)
	buf.Write(ciphertext)

	return buf.Bytes(), nil
}

// DecryptPayload authenticates and decrypts an envelope with the maintainer private key.
func DecryptPayload(privKey *ecdh.PrivateKey, envelope []byte) ([]byte, error) {
	if privKey == nil {
		return nil, errors.New("private key is required for decryption")
	}

	if len(envelope) < MinEnvelopeSize {
		return nil, ErrPayloadTooShort
	}

	if string(envelope[:4]) != EnvelopeMagic {
		return nil, ErrInvalidMagic
	}

	ephemeralPubBytes := envelope[4:36]
	nonce := envelope[36 : 36+NonceSize]
	ciphertext := envelope[36+NonceSize:]

	ephemeralPub, err := ecdh.X25519().NewPublicKey(ephemeralPubBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ephemeral public key in envelope", ErrInvalidKeyFormat)
	}

	sharedSecret, err := privKey.ECDH(ephemeralPub)
	if err != nil {
		return nil, fmt.Errorf("ecdh agreement failed: %w", err)
	}

	aesKey := hkdfSHA256(sharedSecret, nil, []byte(KeyDerivationInfo), 32)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("cipher creation failed: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm creation failed: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(EnvelopeMagic))
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

// hkdfSHA256 provides RFC 5869 HKDF key derivation using HMAC-SHA256.
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	prk := mac.Sum(nil)

	var okm []byte
	var prev []byte
	counter := byte(1)
	for len(okm) < length {
		mac = hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{counter})
		prev = mac.Sum(nil)
		okm = append(okm, prev...)
		counter++
	}
	return okm[:length]
}
