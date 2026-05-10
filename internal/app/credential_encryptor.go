package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

const (
	encryptedCredentialEnvelopeType = "envpilot.credential.v1"
	encryptedCredentialAlgorithm    = "AES-256-GCM"
)

type EncryptedCredential struct {
	Type       string `json:"type"`
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type CredentialEncryptor interface {
	EncryptCredential(ctx context.Context, plaintext []byte) (EncryptedCredential, error)
	DecryptCredential(ctx context.Context, encrypted EncryptedCredential) ([]byte, error)
}

type AESGCMCredentialEncryptor struct {
	key   [32]byte
	keyID string
}

func NewAESGCMCredentialEncryptor(secret string, keyID string) (*AESGCMCredentialEncryptor, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("credential encryption key is required")
	}
	key := sha256.Sum256([]byte(secret))
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		keyID = "local"
	}
	return &AESGCMCredentialEncryptor{key: key, keyID: keyID}, nil
}

func MustNewAESGCMCredentialEncryptor(secret string, keyID string) CredentialEncryptor {
	encryptor, err := NewAESGCMCredentialEncryptor(secret, keyID)
	if err != nil {
		panic(err)
	}
	return encryptor
}

func (e *AESGCMCredentialEncryptor) EncryptCredential(ctx context.Context, plaintext []byte) (EncryptedCredential, error) {
	if err := ctx.Err(); err != nil {
		return EncryptedCredential{}, err
	}
	block, err := aes.NewCipher(e.key[:])
	if err != nil {
		return EncryptedCredential{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedCredential{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptedCredential{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return EncryptedCredential{
		Type:       encryptedCredentialEnvelopeType,
		Version:    1,
		Algorithm:  encryptedCredentialAlgorithm,
		KeyID:      e.keyID,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

func (e *AESGCMCredentialEncryptor) DecryptCredential(ctx context.Context, encrypted EncryptedCredential) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encrypted.Type != encryptedCredentialEnvelopeType {
		return nil, errors.New("unsupported credential envelope")
	}
	if encrypted.Algorithm != encryptedCredentialAlgorithm {
		return nil, errors.New("unsupported credential encryption algorithm")
	}
	nonce, err := base64.StdEncoding.DecodeString(encrypted.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(e.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}
