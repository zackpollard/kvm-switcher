package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitMiddleware_AllowsNormalTraffic(t *testing.T) {
	handler := RateLimitMiddleware(60)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should pass
	req := httptest.NewRequest("POST", "/api/sessions", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRateLimitMiddleware_BlocksExcessTraffic(t *testing.T) {
	// Very low rate: 1 request per minute, burst of 1
	handler := RateLimitMiddleware(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ip := "10.0.0.1:9999"

	// First request should pass
	req := httptest.NewRequest("POST", "/api/sessions", nil)
	req.RemoteAddr = ip
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", w.Code, http.StatusOK)
	}

	// Second request should be rate limited
	req2 := httptest.NewRequest("POST", "/api/sessions", nil)
	req2.RemoteAddr = ip
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want %d", w2.Code, http.StatusTooManyRequests)
	}

	var resp map[string]string
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp["error"] != "too many requests" {
		t.Errorf("error = %q, want 'too many requests'", resp["error"])
	}
	if w2.Header().Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", w2.Header().Get("Retry-After"))
	}
}

func TestRateLimitMiddleware_DifferentIPs(t *testing.T) {
	handler := RateLimitMiddleware(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First IP uses its quota
	req1 := httptest.NewRequest("POST", "/", nil)
	req1.RemoteAddr = "10.0.0.1:1111"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("IP1 first request = %d", w1.Code)
	}

	// Second IP should still have its own quota
	req2 := httptest.NewRequest("POST", "/", nil)
	req2.RemoteAddr = "10.0.0.2:2222"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("IP2 first request = %d, want %d", w2.Code, http.StatusOK)
	}
}
