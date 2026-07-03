// Package tlscert provides TLS certificate sources for the three server modes:
// a locally-generated self-signed CA+leaf (this file, no external deps) and an
// ACME client for Let's Encrypt IP-address certificates (acmeip.go).
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFile   = "selfsigned-ca.crt"
	leafCertFile = "selfsigned-leaf.crt"
	leafKeyFile  = "selfsigned-leaf.key"

	certValidity = 10 * 365 * 24 * time.Hour // long-lived: this is a private trust anchor, no renewal
	renewMargin  = 30 * 24 * time.Hour       // regenerate when less than this remains
)

// SelfSigned holds a leaf certificate (served over TLS) signed by a locally
// generated CA (offered for download so users can trust it once per device).
type SelfSigned struct {
	tlsCert tls.Certificate
	caPEM   []byte
}

// TLSCertificate returns the leaf certificate to install into a tls.Config.
func (s *SelfSigned) TLSCertificate() *tls.Certificate { return &s.tlsCert }

// CACertPEM returns the PEM-encoded local CA certificate for download/install.
func (s *SelfSigned) CACertPEM() []byte { return s.caPEM }

// LoadOrCreateSelfSigned loads a cached self-signed CA+leaf from cacheDir, or
// generates a fresh one. It regenerates when the cached leaf is missing, near
// expiry, or no longer covers every local IP / requested host (e.g. after a
// DHCP address change). extraHosts may contain hostnames and/or IP literals to
// add to the leaf's SANs (a configured domain, WAN IP, listen IP, ...).
func LoadOrCreateSelfSigned(cacheDir string, extraHosts []string) (*SelfSigned, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	hosts, ips := desiredSANs(extraHosts)

	caCertPath := filepath.Join(cacheDir, caCertFile)
	leafCertPath := filepath.Join(cacheDir, leafCertFile)
	leafKeyPath := filepath.Join(cacheDir, leafKeyFile)

	if ss, err := loadSelfSigned(caCertPath, leafCertPath, leafKeyPath, hosts, ips); err == nil {
		return ss, nil
	}
	return createSelfSigned(caCertPath, leafCertPath, leafKeyPath, hosts, ips)
}

// desiredSANs builds the set of SAN entries the leaf must cover: localhost, the
// loopback IPs, any caller-supplied extras (a configured domain / WAN IP), and
// the machine's private IPv4 interface addresses so the server is reachable by
// its LAN IP without configuration.
//
// Auto-detection is deliberately limited to *private IPv4* addresses. Public
// IPs are omitted (to avoid embedding them in a downloadable cert unless the
// operator opts in via config), and IPv6 is omitted because privacy-extension
// addresses rotate — including them would churn the cert daily via covers().
func desiredSANs(extra []string) (hosts []string, ips []net.IP) {
	seenHost := map[string]bool{}
	seenIP := map[string]bool{}
	addHost := func(h string) {
		if h != "" && !seenHost[h] {
			seenHost[h] = true
			hosts = append(hosts, h)
		}
	}
	addIP := func(ip net.IP) {
		if ip == nil {
			return
		}
		if k := ip.String(); !seenIP[k] {
			seenIP[k] = true
			ips = append(ips, ip)
		}
	}

	addHost("localhost")
	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)

	// Explicit extras (domain, WAN IP, ...) are always included as given.
	for _, e := range extra {
		if ip := net.ParseIP(e); ip != nil {
			addIP(ip)
		} else {
			addHost(e)
		}
	}

	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || !ip4.IsPrivate() {
				continue
			}
			addIP(ip4)
		}
	}
	return hosts, ips
}

func loadSelfSigned(caCertPath, leafCertPath, leafKeyPath string, hosts []string, ips []net.IP) (*SelfSigned, error) {
	kp, err := tls.LoadX509KeyPair(leafCertPath, leafKeyPath)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(kp.Certificate[0])
	if err != nil {
		return nil, err
	}
	if time.Now().After(leaf.NotAfter.Add(-renewMargin)) {
		return nil, errors.New("cached leaf near expiry")
	}
	if !covers(leaf, hosts, ips) {
		return nil, errors.New("cached leaf does not cover current SANs")
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, err
	}
	// The served leaf must chain to the CA we hand out at /pintalk-ca.crt;
	// otherwise clients that installed that CA would still get a trust error.
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return nil, errors.New("cached CA is not PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, err
	}
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		return nil, fmt.Errorf("cached leaf does not chain to cached CA: %w", err)
	}
	kp.Leaf = leaf
	return &SelfSigned{tlsCert: kp, caPEM: caPEM}, nil
}

// covers reports whether leaf's SANs include every requested host and IP.
func covers(leaf *x509.Certificate, hosts []string, ips []net.IP) bool {
	haveHost := map[string]bool{}
	for _, h := range leaf.DNSNames {
		haveHost[h] = true
	}
	haveIP := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, h := range hosts {
		if !haveHost[h] {
			return false
		}
	}
	for _, ip := range ips {
		if !haveIP[ip.String()] {
			return false
		}
	}
	return true
}

// createSelfSigned mints a fresh CA and a leaf signed by it. The CA private key
// is generated only in memory and discarded: it is never needed again (the leaf
// and its key are persisted; a cache miss simply mints a new CA+leaf pair).
// Not persisting it keeps a long-lived, device-trusted signing key off disk.
func createSelfSigned(caCertPath, leafCertPath, leafKeyPath string, hosts []string, ips []net.IP) (*SelfSigned, error) {
	now := time.Now()

	// --- local CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pintalk local CA", Organization: []string{"pintalk"}},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	// --- leaf signed by the CA ---
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err = randSerial()
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName(hosts, ips)},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
		IPAddresses:           ips,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, err
	}

	caCertPEM := pemBlock("CERTIFICATE", caDER)
	leafCertPEM := pemBlock("CERTIFICATE", leafDER)
	leafKeyPEM, err := pemKey(leafKey)
	if err != nil {
		return nil, err
	}

	if err := writeFile(caCertPath, caCertPEM, 0o644); err != nil {
		return nil, err
	}
	if err := writeFile(leafCertPath, leafCertPEM, 0o644); err != nil {
		return nil, err
	}
	if err := writeFile(leafKeyPath, leafKeyPEM, 0o600); err != nil {
		return nil, err
	}

	return &SelfSigned{
		tlsCert: tls.Certificate{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey,
			Leaf:        leaf,
		},
		caPEM: caCertPEM,
	}, nil
}

func commonName(hosts []string, ips []net.IP) string {
	// Prefer a real host/IP over the generic "localhost" entry.
	for _, h := range hosts {
		if h != "localhost" {
			return h
		}
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return ip.String()
		}
	}
	return "pintalk"
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pemKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pemBlock("PRIVATE KEY", der), nil
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}
