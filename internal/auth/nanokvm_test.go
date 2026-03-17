package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// TestEvpBytesToKey_KnownVector verifies that evpBytesToKey produces the
// correct key and IV for a known salt+passphrase combination.
func TestEvpBytesToKey_KnownVector(t *testing.T) {
	salt := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	passphrase := []byte("nanokvm-sipeed-2024")

	key, iv := evpBytesToKey(passphrase, salt, 32, 16)

	wantKey := "42c3fe5fe864ee325657646ae8c9d363ba0b3496e5a88c44ba6ce6eea6c952fe"
	wantIV := "116c9a4f3b5668e5f13c47b174267d36"

	if got := hex.EncodeToString(key); got != wantKey {
		t.Errorf("key mismatch\n got: %s\nwant: %s", got, wantKey)
	}
	if got := hex.EncodeToString(iv); got != wantIV {
		t.Errorf("iv mismatch\n got: %s\nwant: %s", got, wantIV)
	}

	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}
	if len(iv) != 16 {
		t.Errorf("iv length = %d, want 16", len(iv))
	}
}

// TestEncryptCryptoJS_OutputFormat verifies the base64-decoded output has the
// OpenSSL-compatible structure: "Salted__" (8 bytes) + salt (8 bytes) +
// ciphertext (multiple of 16 bytes).
func TestEncryptCryptoJS_OutputFormat(t *testing.T) {
	out, err := encryptCryptoJS("hello world", "secret-key")
	if err != nil {
		t.Fatalf("encryptCryptoJS returned error: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}

	// Must be at least 16 bytes: 8 ("Salted__") + 8 (salt) + >=16 (ciphertext)
	if len(raw) < 32 {
		t.Fatalf("decoded length %d < 32 minimum", len(raw))
	}

	// First 8 bytes must be "Salted__"
	if string(raw[:8]) != "Salted__" {
		t.Errorf("prefix = %q, want %q", string(raw[:8]), "Salted__")
	}

	// Ciphertext portion (after 16-byte header) must be a multiple of AES block size (16)
	ciphertext := raw[16:]
	if len(ciphertext)%16 != 0 {
		t.Errorf("ciphertext length %d is not a multiple of 16", len(ciphertext))
	}
}

// TestEncryptCryptoJS_DifferentSalts verifies that two calls with the same
// input produce different outputs because a random salt is generated each time.
func TestEncryptCryptoJS_DifferentSalts(t *testing.T) {
	out1, err := encryptCryptoJS("same-input", "same-key")
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	out2, err := encryptCryptoJS("same-input", "same-key")
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if out1 == out2 {
		t.Error("two encryptions with the same input produced identical output; expected different salts")
	}
}

// TestEncryptCryptoJS_EmptyPlaintext verifies that encrypting an empty string
// produces valid output with the correct OpenSSL format.
func TestEncryptCryptoJS_EmptyPlaintext(t *testing.T) {
	out, err := encryptCryptoJS("", "some-key")
	if err != nil {
		t.Fatalf("encryptCryptoJS returned error: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}

	if string(raw[:8]) != "Salted__" {
		t.Errorf("prefix = %q, want %q", string(raw[:8]), "Salted__")
	}

	// Empty plaintext with PKCS7 padding produces exactly one 16-byte block.
	ciphertext := raw[16:]
	if len(ciphertext) != 16 {
		t.Errorf("ciphertext length = %d, want 16 (one padded AES block)", len(ciphertext))
	}
}

// TestNanoKVMLogin_Success verifies that a successful login returns the
// correct SessionCookie from the token in the JSON response.
func TestNanoKVMLogin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": 0,
			"data": map[string]any{
				"token": "test-jwt",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	creds, err := a.CreateWebSession(context.Background(), host, port, "admin", "admin")
	if err != nil {
		t.Fatalf("CreateWebSession returned error: %v", err)
	}

	if creds.SessionCookie != "test-jwt" {
		t.Errorf("SessionCookie = %q, want %q", creds.SessionCookie, "test-jwt")
	}
}

// TestNanoKVMLogin_BadCredentials verifies that a non-zero code in the
// response produces an error containing "login failed".
func TestNanoKVMLogin_BadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": -2,
			"msg":  "invalid password",
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "wrong")
	if err == nil {
		t.Fatal("expected error for bad credentials, got nil")
	}
	if !strings.Contains(err.Error(), "login failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "login failed")
	}
}

// TestNanoKVMLogin_EmptyToken verifies that a successful code but empty token
// produces an error containing "empty token".
func TestNanoKVMLogin_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": 0,
			"data": map[string]any{
				"token": "",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "admin")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "empty token")
	}
}

// TestNanoKVMLogin_MalformedJSON verifies that a non-JSON response produces
// an error containing "parsing response".
func TestNanoKVMLogin_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("this is not json!!!")); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "admin")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parsing response") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parsing response")
	}
}

// TestNanoKVMLogin_ServerDown verifies that connecting to an unreachable host
// produces an error.
func TestNanoKVMLogin_ServerDown(t *testing.T) {
	a := &NanoKVMAuthenticator{}
	// Use a port that is extremely unlikely to have a listener.
	_, err := a.CreateWebSession(context.Background(), "127.0.0.1", 1, "admin", "admin")
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

// TestNanoKVMAuthenticate_ReturnsCreds verifies that Authenticate returns
// (creds, nil, nil) since NanoKVM has no separate KVM mode.
func TestNanoKVMAuthenticate_ReturnsCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": 0,
			"data": map[string]any{
				"token": "auth-token-123",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	creds, kvmInfo, err := a.Authenticate(context.Background(), host, port, "admin", "admin")
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if kvmInfo != nil {
		t.Errorf("kvmInfo = %+v, want nil", kvmInfo)
	}
	if creds == nil {
		t.Fatal("creds is nil, want non-nil")
	}
	if creds.SessionCookie != "auth-token-123" {
		t.Errorf("SessionCookie = %q, want %q", creds.SessionCookie, "auth-token-123")
	}
}

// TestNanoKVMLogout_SendsToken verifies that Logout sends the nano-kvm-token
// cookie in its request to the server.
func TestNanoKVMLogout_SendsToken(t *testing.T) {
	var receivedCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("nano-kvm-token")
		if err == nil {
			receivedCookie = c.Value
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	err := a.Logout(context.Background(), host, port, &models.BMCCredentials{
		SessionCookie: "tok",
	})
	if err != nil {
		t.Fatalf("Logout returned error: %v", err)
	}
	if receivedCookie != "tok" {
		t.Errorf("received cookie = %q, want %q", receivedCookie, "tok")
	}
}

// TestNanoKVMLogout_EmptyCookie verifies that Logout with an empty
// SessionCookie returns nil without making any HTTP request.
func TestNanoKVMLogout_EmptyCookie(t *testing.T) {
	var requestCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	err := a.Logout(context.Background(), host, port, &models.BMCCredentials{
		SessionCookie: "",
	})
	if err != nil {
		t.Fatalf("Logout returned error: %v", err)
	}
	if n := requestCount.Load(); n != 0 {
		t.Errorf("expected 0 HTTP requests for empty cookie, got %d", n)
	}
}

// TestNanoKVMLogin_EncryptedPasswordSent verifies that the password sent in
// the login request body is an encrypted (URL-encoded base64) value, not the
// plaintext password "admin".
func TestNanoKVMLogin_EncryptedPasswordSent(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		capturedBody = string(bodyBytes)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": 0,
			"data": map[string]any{
				"token": "tok",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	host, port := hostPort(t, srv)
	a := &NanoKVMAuthenticator{}
	_, err := a.CreateWebSession(context.Background(), host, port, "admin", "admin")
	if err != nil {
		t.Fatalf("CreateWebSession returned error: %v", err)
	}

	if capturedBody == "" {
		t.Fatal("no request body captured")
	}

	// Parse the JSON body to extract the password field.
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(capturedBody), &body); err != nil {
		t.Fatalf("failed to parse captured body as JSON: %v", err)
	}

	// The password must NOT be the plaintext value.
	if body.Password == "admin" {
		t.Fatal("password field is plaintext 'admin'; expected encrypted value")
	}

	// URL-decode the password (it was URL-encoded before being put in JSON).
	decoded, err := url.QueryUnescape(body.Password)
	if err != nil {
		t.Fatalf("password is not valid URL-encoding: %v", err)
	}

	// The decoded value should be valid base64.
	raw, err := base64.StdEncoding.DecodeString(decoded)
	if err != nil {
		t.Fatalf("decoded password is not valid base64: %v", err)
	}

	// And it should have the OpenSSL "Salted__" prefix.
	if len(raw) < 16 {
		t.Fatalf("decoded raw length %d < 16", len(raw))
	}
	if string(raw[:8]) != "Salted__" {
		t.Errorf("encrypted password prefix = %q, want %q", string(raw[:8]), "Salted__")
	}
}
