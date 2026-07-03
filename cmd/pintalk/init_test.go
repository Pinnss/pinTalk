package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Pinnss/pinTalk/internal/config"
)

// renderConfig output must be valid YAML and round-trip user-supplied strings
// even when they contain YAML-significant characters (#, :, quotes, ...).
func TestRenderConfigRoundTripsSpecialChars(t *testing.T) {
	v := configValues{
		mode:         config.ModeSelfSigned,
		certCache:    "./certs # tmp",
		turnEnabled:  false,
		realm:        "pintalk",
		secret:       "deadbeef",
		username:     "alice #1: host",
		passwordHash: "$2a$12$b0x77MGOFfK52.r0NNJNCuDpAFgDKcWmv78Y3BySoxI.NdYs1HS0u",
	}
	out := renderConfig(v)

	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("generated config failed to load: %v\n---\n%s", err, out)
	}
	if got := cfg.Hosts[0].Username; got != "alice #1: host" {
		t.Errorf("username = %q, want exact round-trip", got)
	}
	if got := cfg.TLS.CertCache; got != "./certs # tmp" {
		t.Errorf("cert_cache = %q, want exact round-trip", got)
	}
}

// A domain-mode config with a TURN realm/secret must also render+load cleanly.
func TestRenderConfigDomainModeLoads(t *testing.T) {
	v := configValues{
		mode:         config.ModeLetsEncryptDomain,
		domain:       "call.example.com",
		certCache:    "/etc/pintalk/certs",
		turnEnabled:  true,
		turnExternal: "203.0.113.9",
		realm:        "call.example.com",
		secret:       "deadbeefcafe",
		username:     "pin",
		passwordHash: "$2a$12$b0x77MGOFfK52.r0NNJNCuDpAFgDKcWmv78Y3BySoxI.NdYs1HS0u",
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(renderConfig(v)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("domain-mode config failed to load: %v", err)
	}
	if cfg.TLS.Mode != config.ModeLetsEncryptDomain || cfg.Server.Domain != "call.example.com" {
		t.Errorf("unexpected resolved config: mode=%q domain=%q", cfg.TLS.Mode, cfg.Server.Domain)
	}
}
