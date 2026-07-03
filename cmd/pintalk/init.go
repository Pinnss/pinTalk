package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/Pinnss/pinTalk/internal/auth"
	"github.com/Pinnss/pinTalk/internal/config"
)

// cmdInit runs an interactive wizard that writes a ready-to-use config.yaml:
// it picks a TLS mode, hashes the host password, and generates the TURN secret,
// so a non-technical user never has to touch openssl, bcrypt, or YAML by hand.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to write config.yaml")
	flDomain := fs.String("domain", "", "domain (selects letsencrypt-domain mode)")
	flPublicIP := fs.String("public-ip", "", "public IP (for letsencrypt-ip / TURN)")
	flMode := fs.String("mode", "", "tls mode: selfsigned | letsencrypt-domain | letsencrypt-ip")
	flUser := fs.String("username", "", "host username")
	flPass := fs.String("password", "", "host password (omit to be prompted or auto-generated)")
	flCertCache := fs.String("cert-cache", "./certs", "directory for TLS certificates / self-signed CA")
	flYes := fs.Bool("yes", false, "non-interactive: use flags/defaults, never prompt")
	_ = fs.Parse(args)

	in := bufio.NewScanner(os.Stdin)
	interactive := !*flYes

	if _, err := os.Stat(*cfgPath); err == nil {
		if !interactive {
			return fmt.Errorf("%s already exists (refusing to overwrite in --yes mode)", *cfgPath)
		}
		if !askYesNo(in, fmt.Sprintf("%s already exists. Overwrite?", *cfgPath), false) {
			return errors.New("aborted; existing config left untouched")
		}
	}

	lan := detectLANIP()
	domain := strings.TrimSpace(*flDomain)
	publicIP := strings.TrimSpace(*flPublicIP)
	mode := strings.ToLower(strings.TrimSpace(*flMode))

	if mode == "" && interactive {
		fmt.Println()
		fmt.Println("How will people reach this pintalk server?")
		switch {
		case domain != "":
			mode = config.ModeLetsEncryptDomain
		case askYesNo(in, "  Do you have a domain name pointing at this server?", false):
			domain = readLine(in, "  Domain (e.g. call.example.com)", "")
			mode = config.ModeLetsEncryptDomain
		case askYesNo(in, "  Is this server reachable from the internet on a public IP?", false):
			publicIP = readLine(in, "  Public IP (find it with: curl ifconfig.me)", publicIP)
			if askYesNo(in, "  Get a free trusted certificate for that IP from Let's Encrypt?", true) {
				mode = config.ModeLetsEncryptIP
			} else {
				mode = config.ModeSelfSigned
			}
		default:
			mode = config.ModeSelfSigned
			fmt.Printf("  -> Local/LAN mode: self-signed certificate.\n")
		}
	}
	if mode == "" { // non-interactive: infer from flags
		switch {
		case domain != "":
			mode = config.ModeLetsEncryptDomain
		case publicIP != "":
			mode = config.ModeLetsEncryptIP
		default:
			mode = config.ModeSelfSigned
		}
	}
	switch mode {
	case config.ModeSelfSigned, config.ModeLetsEncryptDomain, config.ModeLetsEncryptIP:
	default:
		return fmt.Errorf("invalid mode %q", mode)
	}
	if mode == config.ModeLetsEncryptDomain && domain == "" {
		return errors.New("letsencrypt-domain mode requires a domain")
	}
	if mode == config.ModeLetsEncryptIP && publicIP == "" {
		return errors.New("letsencrypt-ip mode requires a public IP")
	}

	username := strings.TrimSpace(*flUser)
	if username == "" {
		if interactive {
			username = readLine(in, "Host username", "pin")
		} else {
			username = "pin"
		}
	}

	password := *flPass
	generated := false
	if password == "" && interactive {
		password = readLine(in, "Host password (leave empty to auto-generate a strong one)", "")
	}
	if password == "" {
		var err error
		if password, err = genPassword(); err != nil {
			return err
		}
		generated = true
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	secret, err := randHex(32)
	if err != nil {
		return err
	}

	// TURN relays media when a peer is behind symmetric NAT. On a pure LAN it is
	// unnecessary (P2P works directly), so keep it off for self-signed/LAN.
	turnEnabled := mode != config.ModeSelfSigned
	turnExternal := publicIP

	out := renderConfig(configValues{
		mode:         mode,
		domain:       domain,
		publicIP:     publicIP,
		certCache:    *flCertCache,
		turnEnabled:  turnEnabled,
		turnExternal: turnExternal,
		realm:        realmFor(domain, publicIP),
		secret:       secret,
		username:     username,
		passwordHash: hash,
	})
	if err := os.WriteFile(*cfgPath, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *cfgPath, err)
	}

	printSummary(summary{
		mode:      mode,
		domain:    domain,
		publicIP:  publicIP,
		lan:       lan,
		username:  username,
		password:  password,
		generated: generated,
		cfgPath:   *cfgPath,
	})
	return nil
}

type configValues struct {
	mode         string
	domain       string
	publicIP     string
	certCache    string
	turnEnabled  bool
	turnExternal string
	realm        string
	secret       string
	username     string
	passwordHash string
}

func renderConfig(v configValues) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# pintalk config — generated by `pintalk init`.")
	fmt.Fprintln(&b, "# Docs: https://github.com/Pinnss/pinTalk/blob/main/docs/INSTALL.en.md")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "server:")
	fmt.Fprintf(&b, "  domain: %q\n", v.domain)
	fmt.Fprintln(&b, "  listen_ip: \"\"          # \"\" = all interfaces; set to WAN IP if :80/:443 are taken on LAN")
	fmt.Fprintln(&b, "  http_port: 80")
	fmt.Fprintln(&b, "  https_port: 443")
	fmt.Fprintln(&b, "  dev_port: 8080")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "tls:")
	fmt.Fprintf(&b, "  mode: %s\n", v.mode)
	switch v.mode {
	case config.ModeSelfSigned:
		fmt.Fprintln(&b, "  # self-signed: works by IP / on a LAN, no domain needed.")
		fmt.Fprintln(&b, "  # Browsers warn until you install the CA from http://<this-host>/pintalk-ca.crt")
	case config.ModeLetsEncryptDomain:
		fmt.Fprintln(&b, "  # letsencrypt-domain: needs the domain above to resolve to this server, ports 80+443 open.")
	case config.ModeLetsEncryptIP:
		fmt.Fprintln(&b, "  # letsencrypt-ip: trusted 6-day cert for a public IP, auto-renewed. Needs ports 80+443 open.")
	}
	fmt.Fprintf(&b, "  public_ip: %q\n", v.publicIP)
	fmt.Fprintln(&b, "  email: \"\"              # optional ACME account email (letsencrypt modes)")
	fmt.Fprintf(&b, "  cert_cache: %q\n", v.certCache)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "turn:")
	fmt.Fprintf(&b, "  enabled: %t\n", v.turnEnabled)
	fmt.Fprintln(&b, "  listen_ip: 0.0.0.0")
	fmt.Fprintln(&b, "  port: 3478")
	fmt.Fprintf(&b, "  external_ip: %q      # public IP the TURN relay advertises (blank = auto)\n", v.turnExternal)
	fmt.Fprintf(&b, "  realm: %q\n", v.realm)
	fmt.Fprintf(&b, "  shared_secret: %q\n", v.secret)
	fmt.Fprintln(&b, "  cred_ttl_minutes: 60")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "hosts:")
	fmt.Fprintf(&b, "  - username: %q\n", v.username)
	fmt.Fprintf(&b, "    password_hash: %q\n", v.passwordHash)
	return b.String()
}

type summary struct {
	mode      string
	domain    string
	publicIP  string
	lan       string
	username  string
	password  string
	generated bool
	cfgPath   string
}

func printSummary(s summary) {
	fmt.Println()
	fmt.Println("✓ Wrote " + s.cfgPath)
	fmt.Println()
	fmt.Printf("  TLS mode : %s\n", s.mode)
	fmt.Printf("  Login    : %s\n", s.username)
	if s.generated {
		fmt.Printf("  Password : %s   (auto-generated — save it now)\n", s.password)
	} else {
		fmt.Printf("  Password : (the one you entered)\n")
	}
	fmt.Println()

	var url string
	switch s.mode {
	case config.ModeLetsEncryptDomain:
		url = "https://" + s.domain
	case config.ModeLetsEncryptIP:
		url = "https://" + s.publicIP
	default:
		url = "https://" + orPlaceholder(s.lan)
	}
	fmt.Println("Next:")
	fmt.Println("  1. Start the server:   pintalk serve --config " + s.cfgPath)
	fmt.Println("     (ports 80/443 need privileges — use sudo, the systemd service, or")
	fmt.Println("      lower http_port/https_port to 8080/8443 for an unprivileged run.)")
	fmt.Printf("  2. Open %s and log in.\n", url)
	if s.mode == config.ModeSelfSigned {
		fmt.Println("  3. The browser will warn about the certificate — click through to proceed,")
		fmt.Println("     or install the CA once from http://<this-host>/pintalk-ca.crt to remove it.")
	}
	fmt.Println()
}

// --- prompt / detection helpers ---

func readLine(in *bufio.Scanner, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	if !in.Scan() {
		return def
	}
	if s := strings.TrimSpace(in.Text()); s != "" {
		return s
	}
	return def
}

func askYesNo(in *bufio.Scanner, prompt string, def bool) bool {
	opts := "y/N"
	if def {
		opts = "Y/n"
	}
	fmt.Printf("%s [%s]: ", prompt, opts)
	if !in.Scan() {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(in.Text())) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

// detectLANIP returns the primary outbound IPv4 (the LAN address in most setups)
// without sending any packets. Empty string if it can't be determined.
func detectLANIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func realmFor(domain, publicIP string) string {
	if domain != "" {
		return domain
	}
	if publicIP != "" {
		return publicIP
	}
	return "pintalk"
}

func genPassword() (string, error) {
	// Unambiguous alphabet (no 0/O/1/l/I) for something a person can retype.
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func orPlaceholder(ip string) string {
	if ip == "" {
		return "<this-host-ip>"
	}
	return ip
}
