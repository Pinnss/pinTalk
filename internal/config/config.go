package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TLS modes.
const (
	// ModeSelfSigned serves a locally-generated self-signed certificate. Works
	// by IP / on a LAN with no domain; browsers warn until the CA is installed.
	ModeSelfSigned = "selfsigned"
	// ModeLetsEncryptDomain obtains a Let's Encrypt certificate for server.domain
	// via ACME HTTP-01 (the original behaviour). Needs a public domain.
	ModeLetsEncryptDomain = "letsencrypt-domain"
	// ModeLetsEncryptIP obtains a Let's Encrypt certificate for a bare public IP
	// (short-lived "shortlived" profile, auto-renewed). Needs a public IP.
	ModeLetsEncryptIP = "letsencrypt-ip"
)

type Config struct {
	Server ServerConfig  `yaml:"server"`
	TLS    TLSConfig     `yaml:"tls"`
	TURN   TURNConfig    `yaml:"turn"`
	Hosts  []HostAccount `yaml:"hosts"`
}

type ServerConfig struct {
	// Domain — public domain, if you have one. Optional (empty for IP/LAN modes).
	Domain string `yaml:"domain"`
	// ListenIP — which IP to bind HTTP/HTTPS on. Empty = all interfaces (0.0.0.0).
	ListenIP  string `yaml:"listen_ip"`
	HTTPPort  int    `yaml:"http_port"`
	HTTPSPort int    `yaml:"https_port"`
	// CertCache is deprecated: it moved to tls.cert_cache. Still read for
	// backward compatibility with older config files.
	CertCache string `yaml:"cert_cache"`
	DevPort   int    `yaml:"dev_port"`
}

type TLSConfig struct {
	// Mode selects how the HTTPS certificate is obtained. Empty = auto:
	// letsencrypt-domain when server.domain is set, otherwise selfsigned.
	Mode string `yaml:"mode"`
	// PublicIP — the public IP to certify in letsencrypt-ip mode. If empty,
	// falls back to turn.external_ip.
	PublicIP string `yaml:"public_ip"`
	// Email — optional ACME account email (letsencrypt-* modes).
	Email string `yaml:"email"`
	// CertCache — where certificates (and the self-signed CA) are stored.
	CertCache string `yaml:"cert_cache"`
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
	// cert cache: prefer tls.cert_cache, fall back to the legacy server.cert_cache.
	if c.TLS.CertCache == "" {
		c.TLS.CertCache = c.Server.CertCache
	}
	if c.TLS.CertCache == "" {
		c.TLS.CertCache = "./certs"
	}

	if err := c.resolveTLSMode(); err != nil {
		return err
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

	if c.TURN.Enabled {
		if c.TURN.Port == 0 {
			c.TURN.Port = 3478
		}
		if c.TURN.ListenIP == "" {
			c.TURN.ListenIP = "0.0.0.0"
		}
		if c.TURN.Realm == "" {
			c.TURN.Realm = c.defaultRealm()
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

// resolveTLSMode normalises c.TLS.Mode to a concrete mode and checks that the
// mode's prerequisites are present.
func (c *Config) resolveTLSMode() error {
	mode := strings.ToLower(strings.TrimSpace(c.TLS.Mode))
	if mode == "" {
		// Auto: keep the old behaviour (domain => Let's Encrypt) and default to
		// self-signed when no domain is configured.
		if c.Server.Domain != "" {
			mode = ModeLetsEncryptDomain
		} else {
			mode = ModeSelfSigned
		}
	}
	switch mode {
	case ModeSelfSigned:
		// No prerequisites — works by IP / on a LAN.
	case ModeLetsEncryptDomain:
		if c.Server.Domain == "" {
			return errors.New("tls.mode=letsencrypt-domain requires server.domain")
		}
	case ModeLetsEncryptIP:
		ip := c.CertIP()
		if ip == "" {
			return errors.New("tls.mode=letsencrypt-ip requires tls.public_ip (or turn.external_ip)")
		}
		p := net.ParseIP(ip)
		if p == nil {
			return fmt.Errorf("tls.public_ip %q is not a valid IP address", ip)
		}
		// Let's Encrypt only certifies publicly-routable IPs; catch the common
		// misconfiguration (a LAN/loopback address) at config time with a clear
		// message instead of an opaque ACME error at runtime.
		if !p.IsGlobalUnicast() || p.IsPrivate() {
			return fmt.Errorf("tls.mode=letsencrypt-ip needs a public IP; %q is private/reserved and Let's Encrypt will not certify it (use tls.mode=selfsigned instead)", ip)
		}
	default:
		return fmt.Errorf("tls.mode %q is invalid (want: %s | %s | %s)",
			c.TLS.Mode, ModeSelfSigned, ModeLetsEncryptDomain, ModeLetsEncryptIP)
	}
	c.TLS.Mode = mode
	return nil
}

// CertIP returns the public IP to certify in letsencrypt-ip mode.
func (c *Config) CertIP() string {
	if c.TLS.PublicIP != "" {
		return c.TLS.PublicIP
	}
	return c.TURN.ExternalIP
}

// defaultRealm picks a TURN realm when none is configured. The realm is an
// arbitrary label, so any stable string works when there is no domain.
func (c *Config) defaultRealm() string {
	if c.Server.Domain != "" {
		return c.Server.Domain
	}
	if ip := c.CertIP(); ip != "" {
		return ip
	}
	return "pintalk"
}
