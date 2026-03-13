package config

import (
	"fmt"
	"os"

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
	if cfg.Settings.ListenAddress == "" {
		cfg.Settings.ListenAddress = "0.0.0.0:8080"
	}

	for i := range cfg.Servers {
		if cfg.Servers[i].BMCPort <= 0 {
			cfg.Servers[i].BMCPort = 80
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
