package auth

import (
	"testing"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	// The init() functions in each auth file register their authenticators.
	// Verify they're all present.
	expectedTypes := []string{"ami_megarac", "dell_idrac8", "dell_idrac9", "nanokvm", "apc_ups"}
	for _, bt := range expectedTypes {
		auth, ok := Get(bt)
		if !ok {
			t.Errorf("Get(%q) returned ok=false, expected a registered authenticator", bt)
			continue
		}
		if auth == nil {
			t.Errorf("Get(%q) returned nil authenticator", bt)
		}
	}
}

func TestRegistry_GetUnregistered(t *testing.T) {
	auth, ok := Get("nonexistent_board_type")
	if ok {
		t.Error("Get(nonexistent) returned ok=true, expected false")
	}
	if auth != nil {
		t.Error("Get(nonexistent) returned non-nil authenticator")
	}
}
