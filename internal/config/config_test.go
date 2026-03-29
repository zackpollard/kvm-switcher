package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

func TestLoad_MinimalValid(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Errorf("servers = %d, want 1", len(cfg.Servers))
	}
	// Check defaults
	if cfg.Settings.MaxConcurrentSessions != 4 {
		t.Errorf("max_concurrent_sessions = %d, want 4", cfg.Settings.MaxConcurrentSessions)
	}
	if cfg.Servers[0].BMCPort != 80 {
		t.Errorf("bmc_port = %d, want 80", cfg.Servers[0].BMCPort)
	}
}

func TestLoad_NoServers(t *testing.T) {
	path := writeTestConfig(t, `servers: []`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty servers")
	}
	if !strings.Contains(err.Error(), "no servers configured") {
		t.Errorf("error = %q, want 'no servers configured'", err)
	}
}

func TestLoad_DuplicateServerNames(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
  - name: "srv1"
    bmc_ip: "10.0.0.2"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS2"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate names")
	}
	if !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("error = %q, want duplicate name error", err)
	}
}

func TestLoad_MissingServerFields(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "missing name",
			config: `
servers:
  - bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"`,
			wantErr: "name is required",
		},
		{
			name: "missing bmc_ip",
			config: `
servers:
  - name: "srv1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"`,
			wantErr: "bmc_ip is required",
		},
		{
			name: "missing board_type",
			config: `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    username: "admin"
    credential_env: "BMC_PASS"`,
			wantErr: "board_type is required",
		},
		{
			name: "missing username",
			config: `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    credential_env: "BMC_PASS"`,
			wantErr: "username is required",
		},
		{
			name: "missing credential_env",
			config: `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"`,
			wantErr: "credential_env is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.config)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

// OIDC config validation tests

func TestLoad_OIDCDisabled(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
oidc:
  enabled: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.OIDC.Enabled {
		t.Error("OIDC should be disabled")
	}
}

func TestLoad_OIDCMissingFields(t *testing.T) {
	base := `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
oidc:
  enabled: true
`
	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:    "missing issuer_url",
			extra:   "  client_id: x\n  client_secret_env: X\n  redirect_url: http://x\n  role_mappings:\n    admin:\n      servers: ['*']",
			wantErr: "issuer_url is required",
		},
		{
			name:    "missing client_id",
			extra:   "  issuer_url: https://x\n  client_secret_env: X\n  redirect_url: http://x\n  role_mappings:\n    admin:\n      servers: ['*']",
			wantErr: "client_id is required",
		},
		{
			name:    "missing client_secret_env",
			extra:   "  issuer_url: https://x\n  client_id: x\n  redirect_url: http://x\n  role_mappings:\n    admin:\n      servers: ['*']",
			wantErr: "client_secret_env is required",
		},
		{
			name:    "missing redirect_url",
			extra:   "  issuer_url: https://x\n  client_id: x\n  client_secret_env: X\n  role_mappings:\n    admin:\n      servers: ['*']",
			wantErr: "redirect_url is required",
		},
		{
			name:    "missing role_mappings",
			extra:   "  issuer_url: https://x\n  client_id: x\n  client_secret_env: X\n  redirect_url: http://x",
			wantErr: "at least one role_mapping is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, base+tt.extra)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_OIDCUnknownServerInMapping(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
oidc:
  enabled: true
  issuer_url: "https://auth.example.com"
  client_id: "test"
  client_secret_env: "SECRET"
  redirect_url: "http://localhost/callback"
  role_mappings:
    admin:
      servers: ["nonexistent-server"]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown server in mapping")
	}
	if !strings.Contains(err.Error(), "unknown server") {
		t.Errorf("error = %q, want unknown server error", err)
	}
}

func TestLoad_OIDCWildcardAllowed(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
oidc:
  enabled: true
  issuer_url: "https://auth.example.com"
  client_id: "test"
  client_secret_env: "SECRET"
  redirect_url: "http://localhost/callback"
  role_mappings:
    admin:
      servers: ["*"]
`)
	// This should pass validation (wildcard is allowed even though "*" isn't a server name)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.OIDC.Enabled {
		t.Error("OIDC should be enabled")
	}
	if len(cfg.OIDC.RoleMappings) != 1 {
		t.Errorf("role_mappings = %d, want 1", len(cfg.OIDC.RoleMappings))
	}
}

func TestLoad_OIDCValidServerMapping(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
  - name: "srv2"
    bmc_ip: "10.0.0.2"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS2"
oidc:
  enabled: true
  issuer_url: "https://auth.example.com"
  client_id: "test"
  client_secret_env: "SECRET"
  redirect_url: "http://localhost/callback"
  role_claim: "roles"
  scopes: ["openid", "profile", "roles"]
  role_mappings:
    admin:
      servers: ["*"]
    ops:
      servers: ["srv1", "srv2"]
    dev:
      servers: ["srv1"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.OIDC.RoleClaim != "roles" {
		t.Errorf("role_claim = %q, want roles", cfg.OIDC.RoleClaim)
	}
	if len(cfg.OIDC.Scopes) != 3 {
		t.Errorf("scopes = %d, want 3", len(cfg.OIDC.Scopes))
	}
	if len(cfg.OIDC.RoleMappings) != 3 {
		t.Errorf("role_mappings = %d, want 3", len(cfg.OIDC.RoleMappings))
	}
}

func TestLoad_NewSettingsDefaults(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Settings.CORSOrigins) != 1 || cfg.Settings.CORSOrigins[0] != "*" {
		t.Errorf("CORSOrigins = %v, want [*]", cfg.Settings.CORSOrigins)
	}
	if cfg.Settings.RateLimitRPM != 60 {
		t.Errorf("RateLimitRPM = %d, want 60", cfg.Settings.RateLimitRPM)
	}
	if cfg.Settings.DBPath != "data/kvm-switcher.db" {
		t.Errorf("DBPath = %q, want data/kvm-switcher.db", cfg.Settings.DBPath)
	}
	if cfg.Settings.AuditLog == nil || !*cfg.Settings.AuditLog {
		t.Errorf("AuditLog = %v, want true", cfg.Settings.AuditLog)
	}
	if cfg.Settings.MetricsEnabled {
		t.Errorf("MetricsEnabled = %v, want false", cfg.Settings.MetricsEnabled)
	}
	if cfg.Settings.BMCCredsTTLMinutes != 120 {
		t.Errorf("BMCCredsTTLMinutes = %d, want 120", cfg.Settings.BMCCredsTTLMinutes)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	path := writeTestConfig(t, `
servers:
  - name: "srv1"
    bmc_ip: "10.0.0.1"
    board_type: "ami_megarac"
    username: "admin"
    credential_env: "BMC_PASS"
`)
	t.Setenv("KVM_CORS_ORIGINS", "https://a.com,https://b.com")
	t.Setenv("KVM_RATE_LIMIT_RPM", "120")
	t.Setenv("KVM_DB_PATH", "/tmp/test.db")
	t.Setenv("KVM_AUDIT_LOG", "false")
	t.Setenv("KVM_METRICS_ENABLED", "true")
	t.Setenv("KVM_BMC_CREDS_TTL_MINUTES", "30")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Settings.CORSOrigins) != 2 || cfg.Settings.CORSOrigins[0] != "https://a.com" {
		t.Errorf("CORSOrigins = %v, want [https://a.com https://b.com]", cfg.Settings.CORSOrigins)
	}
	if cfg.Settings.RateLimitRPM != 120 {
		t.Errorf("RateLimitRPM = %d, want 120", cfg.Settings.RateLimitRPM)
	}
	if cfg.Settings.DBPath != "/tmp/test.db" {
		t.Errorf("DBPath = %q, want /tmp/test.db", cfg.Settings.DBPath)
	}
	if cfg.Settings.AuditLog == nil || *cfg.Settings.AuditLog {
		t.Errorf("AuditLog = %v, want false", cfg.Settings.AuditLog)
	}
	if !cfg.Settings.MetricsEnabled {
		t.Errorf("MetricsEnabled = %v, want true", cfg.Settings.MetricsEnabled)
	}
	if cfg.Settings.BMCCredsTTLMinutes != 30 {
		t.Errorf("BMCCredsTTLMinutes = %d, want 30", cfg.Settings.BMCCredsTTLMinutes)
	}
}

func TestGetPassword(t *testing.T) {
	t.Run("password set", func(t *testing.T) {
		t.Setenv("TEST_BMC_PASS", "secret123")
		server := &models.ServerConfig{Name: "srv1", CredentialEnv: "TEST_BMC_PASS"}
		pass, err := GetPassword(server)
		if err != nil {
			t.Fatalf("GetPassword() error: %v", err)
		}
		if pass != "secret123" {
			t.Errorf("password = %q, want secret123", pass)
		}
	})

	t.Run("password not set", func(t *testing.T) {
		server := &models.ServerConfig{Name: "srv1", CredentialEnv: "UNSET_ENV_VAR_FOR_TEST"}
		os.Unsetenv("UNSET_ENV_VAR_FOR_TEST")
		_, err := GetPassword(server)
		if err == nil {
			t.Fatal("expected error for unset env var")
		}
	})
}
