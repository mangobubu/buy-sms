package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		h := sha256.Sum256(key)
		key = h[:]
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: a}, nil
}

func (v *Vault) Encrypt(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (v *Vault) Decrypt(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	n := v.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("密文损坏")
	}
	p, err := v.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", errors.New("密钥数据解密失败")
	}
	return string(p), nil
}
