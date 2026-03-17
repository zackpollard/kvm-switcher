package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/zackpollard/kvm-switcher/internal/models"
	"gopkg.in/yaml.v3"
)

// Load reads and parses the YAML configuration file.
func Load(path string) (*models.AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg models.AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	setDefaults(&cfg)
	applyEnvOverrides(&cfg)

	return &cfg, nil
}

func validate(cfg *models.AppConfig) error {
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("no servers configured")
	}

	names := make(map[string]bool)
	for i, s := range cfg.Servers {
		if s.Name == "" {
			return fmt.Errorf("server %d: name is required", i)
		}
		if names[s.Name] {
			return fmt.Errorf("server %d: duplicate name %q", i, s.Name)
		}
		names[s.Name] = true

		if s.BMCIP == "" {
			return fmt.Errorf("server %q: bmc_ip is required", s.Name)
		}
		if s.BoardType == "" {
			return fmt.Errorf("server %q: board_type is required", s.Name)
		}
		if s.Username == "" {
			return fmt.Errorf("server %q: username is required", s.Name)
		}
		if s.CredentialEnv == "" {
			return fmt.Errorf("server %q: credential_env is required", s.Name)
		}
	}

	switch cfg.Settings.Runtime {
	case "", "docker", "kubernetes":
		// valid
	default:
		return fmt.Errorf("unknown runtime %q (must be \"docker\" or \"kubernetes\")", cfg.Settings.Runtime)
	}

	if cfg.OIDC.Enabled {
		if cfg.OIDC.IssuerURL == "" {
			return fmt.Errorf("oidc: issuer_url is required when OIDC is enabled")
		}
		if cfg.OIDC.ClientID == "" {
			return fmt.Errorf("oidc: client_id is required when OIDC is enabled")
		}
		if cfg.OIDC.ClientSecretEnv == "" {
			return fmt.Errorf("oidc: client_secret_env is required when OIDC is enabled")
		}
		if cfg.OIDC.RedirectURL == "" {
			return fmt.Errorf("oidc: redirect_url is required when OIDC is enabled")
		}
		if len(cfg.OIDC.RoleMappings) == 0 {
			return fmt.Errorf("oidc: at least one role_mapping is required when OIDC is enabled")
		}
		// Validate that mapped servers exist (unless wildcard)
		serverNames := make(map[string]bool)
		for _, s := range cfg.Servers {
			serverNames[s.Name] = true
		}
		for role, mapping := range cfg.OIDC.RoleMappings {
			for _, srv := range mapping.Servers {
				if srv != "*" && !serverNames[srv] {
					return fmt.Errorf("oidc: role %q references unknown server %q", role, srv)
				}
			}
		}
	}

	return nil
}

func setDefaults(cfg *models.AppConfig) {
	if cfg.Settings.MaxConcurrentSessions <= 0 {
		cfg.Settings.MaxConcurrentSessions = 4
	}
	if cfg.Settings.SessionTimeoutMinutes <= 0 {
		cfg.Settings.SessionTimeoutMinutes = 60
	}
	if cfg.Settings.IdleTimeoutMinutes <= 0 {
		cfg.Settings.IdleTimeoutMinutes = 30
	}
	if cfg.Settings.DockerImage == "" {
		cfg.Settings.DockerImage = "kvm-switcher/jviewer:latest"
	}
	if cfg.Settings.ContainerImage == "" {
		cfg.Settings.ContainerImage = cfg.Settings.DockerImage
	}
	if cfg.Settings.Runtime == "" {
		cfg.Settings.Runtime = "docker"
	}
	if cfg.Settings.KubeNamespace == "" {
		cfg.Settings.KubeNamespace = "kvm-switcher"
	}
	if cfg.Settings.ListenAddress == "" {
		cfg.Settings.ListenAddress = "0.0.0.0:8080"
	}

	if len(cfg.Settings.CORSOrigins) == 0 {
		cfg.Settings.CORSOrigins = []string{"*"}
	}
	if cfg.Settings.RateLimitRPM <= 0 {
		cfg.Settings.RateLimitRPM = 60
	}
	if cfg.Settings.DBPath == "" {
		cfg.Settings.DBPath = "data/kvm-switcher.db"
	}
	if cfg.Settings.AuditLog == nil {
		t := true
		cfg.Settings.AuditLog = &t
	}
	if cfg.Settings.BMCCredsTTLMinutes <= 0 {
		cfg.Settings.BMCCredsTTLMinutes = 120
	}

	for i := range cfg.Servers {
		if cfg.Servers[i].BMCPort <= 0 {
			switch cfg.Servers[i].BoardType {
			case "dell_idrac8", "dell_idrac9":
				cfg.Servers[i].BMCPort = 443
			default:
				cfg.Servers[i].BMCPort = 80
			}
		}
	}
}

// applyEnvOverrides reads environment variables and overrides corresponding settings.
func applyEnvOverrides(cfg *models.AppConfig) {
	if v := os.Getenv("KVM_CORS_ORIGINS"); v != "" {
		cfg.Settings.CORSOrigins = strings.Split(v, ",")
	}
	if v := os.Getenv("KVM_RATE_LIMIT_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Settings.RateLimitRPM = n
		}
	}
	if v := os.Getenv("KVM_DB_PATH"); v != "" {
		cfg.Settings.DBPath = v
	}
	if v := os.Getenv("KVM_AUDIT_LOG"); v != "" {
		b := v == "true" || v == "1"
		cfg.Settings.AuditLog = &b
	}
	if v := os.Getenv("KVM_METRICS_ENABLED"); v != "" {
		cfg.Settings.MetricsEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("KVM_BMC_CREDS_TTL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Settings.BMCCredsTTLMinutes = n
		}
	}
}

// GetPassword retrieves the BMC password for a server from environment variables.
func GetPassword(server *models.ServerConfig) (string, error) {
	password := os.Getenv(server.CredentialEnv)
	if password == "" {
		return "", fmt.Errorf("environment variable %s is not set for server %q", server.CredentialEnv, server.Name)
	}
	return password, nil
}
