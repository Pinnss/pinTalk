package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validHash = "$2a$12$b0x77MGOFfK52.r0NNJNCuDpAFgDKcWmv78Y3BySoxI.NdYs1HS0u"

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// Old configs (server.domain set, no tls block, cert_cache under server) must
// keep working and map to letsencrypt-domain with the legacy cert cache.
func TestBackwardCompatDomainOnly(t *testing.T) {
	cfg, err := loadYAML(t, `
server:
  domain: call.example.com
  cert_cache: /etc/pintalk/certs
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLS.Mode != ModeLetsEncryptDomain {
		t.Errorf("mode = %q, want %q", cfg.TLS.Mode, ModeLetsEncryptDomain)
	}
	if cfg.TLS.CertCache != "/etc/pintalk/certs" {
		t.Errorf("cert cache = %q, want legacy server.cert_cache", cfg.TLS.CertCache)
	}
}

func TestSelfSignedIsDefaultWithoutDomain(t *testing.T) {
	cfg, err := loadYAML(t, `
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLS.Mode != ModeSelfSigned {
		t.Errorf("mode = %q, want %q", cfg.TLS.Mode, ModeSelfSigned)
	}
	if cfg.TLS.CertCache != "./certs" {
		t.Errorf("cert cache = %q, want ./certs", cfg.TLS.CertCache)
	}
}

func TestLetsEncryptDomainRequiresDomain(t *testing.T) {
	_, err := loadYAML(t, `
tls:
  mode: letsencrypt-domain
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err == nil || !strings.Contains(err.Error(), "requires server.domain") {
		t.Errorf("want domain-required error, got %v", err)
	}
}

func TestLetsEncryptIPRequiresIP(t *testing.T) {
	_, err := loadYAML(t, `
tls:
  mode: letsencrypt-ip
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err == nil || !strings.Contains(err.Error(), "letsencrypt-ip") {
		t.Errorf("want ip-required error, got %v", err)
	}
}

func TestLetsEncryptIPFallsBackToTurnExternalIP(t *testing.T) {
	cfg, err := loadYAML(t, `
tls:
  mode: letsencrypt-ip
turn:
  enabled: true
  external_ip: "203.0.113.9"
  shared_secret: "deadbeef"
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.CertIP(); got != "203.0.113.9" {
		t.Errorf("CertIP = %q, want turn.external_ip fallback", got)
	}
}

func TestPublicIPPrefersTLSOverTurn(t *testing.T) {
	cfg, err := loadYAML(t, `
tls:
  mode: letsencrypt-ip
  public_ip: "198.51.100.5"
turn:
  enabled: true
  external_ip: "203.0.113.9"
  shared_secret: "deadbeef"
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.CertIP(); got != "198.51.100.5" {
		t.Errorf("CertIP = %q, want tls.public_ip precedence", got)
	}
}

func TestLetsEncryptIPRejectsNonPublicIP(t *testing.T) {
	for _, ip := range []string{"192.168.1.50", "127.0.0.1", "10.0.0.1", "169.254.1.1"} {
		_, err := loadYAML(t, `
tls:
  mode: letsencrypt-ip
  public_ip: "`+ip+`"
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
		if err == nil || !strings.Contains(err.Error(), "public IP") {
			t.Errorf("ip %s: want public-IP rejection, got %v", ip, err)
		}
	}
}

func TestLetsEncryptIPAcceptsPublicIP(t *testing.T) {
	cfg, err := loadYAML(t, `
tls:
  mode: letsencrypt-ip
  public_ip: "203.0.113.9"
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("public IP rejected: %v", err)
	}
	if cfg.CertIP() != "203.0.113.9" {
		t.Errorf("CertIP = %q", cfg.CertIP())
	}
}

func TestInvalidModeRejected(t *testing.T) {
	_, err := loadYAML(t, `
tls:
  mode: banana
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("want invalid-mode error, got %v", err)
	}
}

func TestSelfSignedRealmDefaultsToPintalk(t *testing.T) {
	cfg, err := loadYAML(t, `
tls:
  mode: selfsigned
turn:
  enabled: true
  shared_secret: "deadbeef"
hosts:
  - username: pin
    password_hash: "`+validHash+`"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TURN.Realm != "pintalk" {
		t.Errorf("realm = %q, want pintalk", cfg.TURN.Realm)
	}
}
