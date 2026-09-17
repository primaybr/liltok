package crypto

import (
	"bytes"
	"testing"
)

func TestKeyPairGenerationAndParsing(t *testing.T) {
	pair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	if len(pair.PrivateKeyStr) == 0 || len(pair.PublicKeyStr) == 0 {
		t.Fatalf("expected non-empty key strings")
	}

	parsedPub, err := ParsePublicKey(pair.PublicKeyStr)
	if err != nil {
		t.Fatalf("ParsePublicKey failed: %v", err)
	}

	if !bytes.Equal(parsedPub.Bytes(), pair.PublicKey.Bytes()) {
		t.Fatalf("parsed public key mismatch")
	}

	parsedPriv, err := ParsePrivateKey(pair.PrivateKeyStr)
	if err != nil {
		t.Fatalf("ParsePrivateKey failed: %v", err)
	}

	if !bytes.Equal(parsedPriv.Bytes(), pair.PrivateKey.Bytes()) {
		t.Fatalf("parsed private key mismatch")
	}
}

func TestEncryptAndDecryptRoundTrip(t *testing.T) {
	pair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	plaintext := []byte("hello world! this is a test cache payload for liltok.")

	envelope, err := EncryptPayload(pair.PublicKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptPayload failed: %v", err)
	}

	if len(envelope) <= len(plaintext) {
		t.Fatalf("envelope must be larger than plaintext")
	}

	decrypted, err := DecryptPayload(pair.PrivateKey, envelope)
	if err != nil {
		t.Fatalf("DecryptPayload failed: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted text does not match original plaintext")
	}
}

func TestTamperedCiphertextRejection(t *testing.T) {
	pair, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	plaintext := []byte("secret cache entry")
	envelope, err := EncryptPayload(pair.PublicKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptPayload failed: %v", err)
	}

	// Tamper with ciphertext byte
	envelope[len(envelope)-1] ^= 0xFF

	_, err = DecryptPayload(pair.PrivateKey, envelope)
	if err == nil {
		t.Fatalf("expected decryption error on tampered envelope")
	}
}

func TestWrongPrivateKeyRejection(t *testing.T) {
	pair1, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	pair2, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	plaintext := []byte("private cache entry")
	envelope, err := EncryptPayload(pair1.PublicKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptPayload failed: %v", err)
	}

	_, err = DecryptPayload(pair2.PrivateKey, envelope)
	if err == nil {
		t.Fatalf("expected decryption error with wrong private key")
	}
}
