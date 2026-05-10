package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig  `yaml:"server"`
	TURN   TURNConfig    `yaml:"turn"`
	Hosts  []HostAccount `yaml:"hosts"`
}

type ServerConfig struct {
	Domain    string `yaml:"domain"`
	// ListenIP — на каком IP слушать HTTP/HTTPS. Пусто = все интерфейсы (0.0.0.0).
	// Поставь WAN-IP роутера, если на 80/443 ещё стоит LuCI на LAN-IP.
	ListenIP  string `yaml:"listen_ip"`
	HTTPPort  int    `yaml:"http_port"`
	HTTPSPort int    `yaml:"https_port"`
	CertCache string `yaml:"cert_cache"`
	DevPort   int    `yaml:"dev_port"`
}

type TURNConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ListenIP       string `yaml:"listen_ip"`
	Port           int    `yaml:"port"`
	ExternalIP     string `yaml:"external_ip"`
	Realm          string `yaml:"realm"`
	SharedSecret   string `yaml:"shared_secret"`
	CredTTLMinutes int    `yaml:"cred_ttl_minutes"`
}

type HostAccount struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Domain == "" {
		return errors.New("server.domain is required")
	}
	if c.Server.HTTPPort == 0 {
		c.Server.HTTPPort = 80
	}
	if c.Server.HTTPSPort == 0 {
		c.Server.HTTPSPort = 443
	}
	if c.Server.DevPort == 0 {
		c.Server.DevPort = 8080
	}
	if c.Server.CertCache == "" {
		c.Server.CertCache = "./certs"
	}
	if c.TURN.Enabled {
		if c.TURN.Port == 0 {
			c.TURN.Port = 3478
		}
		if c.TURN.ListenIP == "" {
			c.TURN.ListenIP = "0.0.0.0"
		}
		if c.TURN.Realm == "" {
			c.TURN.Realm = c.Server.Domain
		}
		if c.TURN.SharedSecret == "" || strings.Contains(c.TURN.SharedSecret, "ЗАМЕНИ") {
			return errors.New("turn.shared_secret must be set to a real random string")
		}
		if c.TURN.CredTTLMinutes == 0 {
			c.TURN.CredTTLMinutes = 60
		}
	}
	if len(c.Hosts) == 0 {
		return errors.New("at least one host account required")
	}
	for i, h := range c.Hosts {
		if h.Username == "" {
			return fmt.Errorf("hosts[%d].username is required", i)
		}
		if !strings.HasPrefix(h.PasswordHash, "$2") {
			return fmt.Errorf("hosts[%d].password_hash must be a bcrypt hash", i)
		}
	}
	return nil
}
