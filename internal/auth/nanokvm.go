package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func init() {
	Register("nanokvm", &NanoKVMAuthenticator{})
}

const nanoKVMEncryptKey = "nanokvm-sipeed-2024"

// NanoKVMAuthenticator handles authentication for Sipeed NanoKVM devices.
// The NanoKVM web UI is itself the KVM client (MJPEG video + WebSocket HID),
// so there's no separate KVM session — just proxy the web UI with the auth token.
type NanoKVMAuthenticator struct{}

func (a *NanoKVMAuthenticator) Authenticate(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, *models.KVMConnectInfo, error) {
	// NanoKVM's web UI IS the KVM client — no separate KVM mode needed.
	// Return the web session credentials; the frontend opens the proxied
	// NanoKVM UI directly.
	creds, err := a.CreateWebSession(ctx, host, port, username, password)
	if err != nil {
		return nil, nil, err
	}
	return creds, nil, nil
}

func (a *NanoKVMAuthenticator) CreateWebSession(ctx context.Context, host string, port int, username, password string) (*models.BMCCredentials, error) {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)

	token, err := a.login(ctx, baseURL, username, password)
	if err != nil {
		return nil, fmt.Errorf("NanoKVM login: %w", err)
	}

	log.Printf("NanoKVM %s: authenticated", host)

	return &models.BMCCredentials{
		SessionCookie: token,
	}, nil
}

func (a *NanoKVMAuthenticator) Logout(ctx context.Context, host string, port int, creds *models.BMCCredentials) error {
	if creds.SessionCookie == "" {
		return nil
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/auth/logout", nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "nano-kvm-token", Value: creds.SessionCookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type nanoKVMLoginResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}

func (a *NanoKVMAuthenticator) login(ctx context.Context, baseURL, username, password string) (string, error) {
	// NanoKVM requires AES-256-CBC encryption of the password using CryptoJS
	// format (OpenSSL-compatible: "Salted__" + 8-byte salt + ciphertext).
	encPassword, err := encryptCryptoJS(password, nanoKVMEncryptKey)
	if err != nil {
		return "", fmt.Errorf("encrypting password: %w", err)
	}

	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`,
		username, url.QueryEscape(encPassword))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/auth/login",
		strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	var loginResp nanoKVMLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	if loginResp.Code != 0 {
		return "", fmt.Errorf("login failed: %s", loginResp.Msg)
	}

	if loginResp.Data.Token == "" {
		return "", fmt.Errorf("login returned empty token")
	}

	return loginResp.Data.Token, nil
}

// encryptCryptoJS encrypts plaintext using CryptoJS-compatible AES-256-CBC.
// CryptoJS with a string passphrase uses EVP_BytesToKey to derive key+IV,
// and outputs OpenSSL format: base64("Salted__" + salt + ciphertext).
func encryptCryptoJS(plaintext, passphrase string) (string, error) {
	salt := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}

	key, iv := evpBytesToKey([]byte(passphrase), salt, 32, 16)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	// PKCS7 padding
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	// OpenSSL format: "Salted__" + salt + ciphertext
	result := make([]byte, 0, 8+8+len(ciphertext))
	result = append(result, []byte("Salted__")...)
	result = append(result, salt...)
	result = append(result, ciphertext...)

	return base64.StdEncoding.EncodeToString(result), nil
}

// evpBytesToKey derives key and IV from a passphrase and salt using
// the OpenSSL EVP_BytesToKey algorithm (MD5-based).
func evpBytesToKey(password, salt []byte, keyLen, ivLen int) ([]byte, []byte) {
	var derived []byte
	var block []byte
	for len(derived) < keyLen+ivLen {
		h := md5.New()
		h.Write(block)
		h.Write(password)
		h.Write(salt)
		block = h.Sum(nil)
		derived = append(derived, block...)
	}
	return derived[:keyLen], derived[keyLen : keyLen+ivLen]
}
