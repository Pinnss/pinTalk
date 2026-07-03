package tlscert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

const (
	// LEDirectoryProduction is Let's Encrypt's production ACME endpoint. Override
	// with the PINTALK_ACME_DIRECTORY env var (e.g. to point at staging).
	leProduction = "https://acme-v02.api.letsencrypt.org/directory"

	// shortlivedProfile is the ACME profile Let's Encrypt requires for IP certs.
	shortlivedProfile = "shortlived"

	acmeChallengePrefix = "/.well-known/acme-challenge/"

	accountKeyFile = "acme-account.key"
	ipCertFile     = "acme-ip.crt"
	ipKeyFile      = "acme-ip.key"

	renewBefore   = 48 * time.Hour // IP certs live ~6 days; renew when 2 remain
	renewRetry    = time.Hour      // retry interval after a failed obtain/renew
	obtainTries   = 3              // initial obtain attempts before giving up
	obtainBackoff = 30 * time.Second
)

// ACMEManager obtains and renews Let's Encrypt certificates for a bare IP
// address using the ACME HTTP-01 challenge and the "shortlived" profile. It
// implements challenge.Provider (Present/CleanUp) itself, serving tokens via
// HTTPHandler, and hands the current certificate to a tls.Config through
// GetCertificate.
type ACMEManager struct {
	domains   []string // the IP(s) to certify
	email     string
	cacheDir  string
	directory string
	logger    *log.Logger

	mu      sync.RWMutex
	current *tls.Certificate

	tokMu  sync.RWMutex
	tokens map[string]string // http-01 token -> keyAuth
}

// NewACMEManager builds a manager for the given IP. directory is the ACME
// endpoint; pass "" for Let's Encrypt production (overridable via
// PINTALK_ACME_DIRECTORY).
func NewACMEManager(ip, email, cacheDir string, logger *log.Logger) *ACMEManager {
	directory := os.Getenv("PINTALK_ACME_DIRECTORY")
	if directory == "" {
		directory = leProduction
	}
	return &ACMEManager{
		domains:   []string{ip},
		email:     email,
		cacheDir:  cacheDir,
		directory: directory,
		logger:    logger,
		tokens:    make(map[string]string),
	}
}

// Start ensures a usable certificate is available (loading a cached one or
// obtaining a fresh one) and launches the background renewal loop. The HTTP-01
// listener (see HTTPHandler) MUST already be serving on port 80 before Start is
// called, because obtaining a certificate triggers a challenge fetch.
func (m *ACMEManager) Start(ctx context.Context) error {
	if err := m.loadCachedCert(); err != nil {
		// A missing cache is the normal first run; anything else (corrupt file,
		// key/cert mismatch, transient IO) is worth surfacing rather than silently
		// re-hitting Let's Encrypt as if nothing were cached.
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Printf("acme: cached certificate unusable (%v); obtaining a new one", err)
		}
		m.logger.Printf("acme: obtaining certificate for %s ...", strings.Join(m.domains, ", "))
		if err := m.obtainWithRetry(ctx, obtainTries); err != nil {
			return fmt.Errorf("obtain certificate: %w", err)
		}
	} else {
		m.logger.Printf("acme: loaded cached certificate for %s (expires %s)",
			strings.Join(m.domains, ", "), m.notAfter().Format(time.RFC3339))
	}
	go m.renewLoop(ctx)
	return nil
}

// GetCertificate is a tls.Config.GetCertificate callback returning the current
// certificate.
func (m *ACMEManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil, errors.New("no certificate available yet")
	}
	return m.current, nil
}

// HTTPHandler serves ACME HTTP-01 challenge responses and delegates all other
// requests to fallback (typically a redirect-to-HTTPS handler).
func (m *ACMEManager) HTTPHandler(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acmeChallengePrefix) {
			token := strings.TrimPrefix(r.URL.Path, acmeChallengePrefix)
			m.tokMu.RLock()
			keyAuth, ok := m.tokens[token]
			m.tokMu.RUnlock()
			if ok {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(keyAuth))
				return
			}
			http.NotFound(w, r)
			return
		}
		if fallback != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

// Present / CleanUp implement lego's challenge.Provider for HTTP-01.

func (m *ACMEManager) Present(_, token, keyAuth string) error {
	m.tokMu.Lock()
	m.tokens[token] = keyAuth
	m.tokMu.Unlock()
	return nil
}

func (m *ACMEManager) CleanUp(_, token, _ string) error {
	m.tokMu.Lock()
	delete(m.tokens, token)
	m.tokMu.Unlock()
	return nil
}

// --- internals ---

func (m *ACMEManager) renewLoop(ctx context.Context) {
	for {
		wait := m.timeUntilRenew()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := m.obtainWithRetry(ctx, 1); err != nil {
			m.logger.Printf("acme: renewal failed (%v), retrying in %s", err, renewRetry)
			select {
			case <-ctx.Done():
				return
			case <-time.After(renewRetry):
			}
		} else {
			m.logger.Printf("acme: renewed certificate for %s (expires %s)",
				strings.Join(m.domains, ", "), m.notAfter().Format(time.RFC3339))
		}
	}
}

func (m *ACMEManager) timeUntilRenew() time.Duration {
	na := m.notAfter()
	if na.IsZero() {
		return 0
	}
	if d := time.Until(na.Add(-renewBefore)); d > 0 {
		return d
	}
	return 0
}

func (m *ACMEManager) notAfter() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil || m.current.Leaf == nil {
		return time.Time{}
	}
	return m.current.Leaf.NotAfter
}

func (m *ACMEManager) obtainWithRetry(ctx context.Context, tries int) error {
	var lastErr error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(obtainBackoff):
			}
		}
		// lego's Obtain takes no context and can't be interrupted mid-flight, so
		// run it in a goroutine and race it against ctx: on shutdown Start /
		// renewLoop return promptly while the in-flight request unwinds on lego's
		// own timeout. The channel is buffered so that goroutine never leaks.
		done := make(chan error, 1)
		go func() { done <- m.obtain() }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err != nil {
				lastErr = err
				m.logger.Printf("acme: obtain attempt %d/%d failed: %v", i+1, tries, err)
				continue
			}
			return nil
		}
	}
	return lastErr
}

func (m *ACMEManager) obtain() error {
	client, err := m.newClient()
	if err != nil {
		return err
	}
	if err := client.Challenge.SetHTTP01Provider(m); err != nil {
		return err
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: m.domains,
		Bundle:  true,
		Profile: shortlivedProfile,
	})
	if err != nil {
		return err
	}

	if err := writeFile(filepath.Join(m.cacheDir, ipCertFile), res.Certificate, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(m.cacheDir, ipKeyFile), res.PrivateKey, 0o600); err != nil {
		return err
	}
	return m.setCert(res.Certificate, res.PrivateKey)
}

func (m *ACMEManager) setCert(certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	cert.Leaf = leaf
	m.mu.Lock()
	m.current = &cert
	m.mu.Unlock()
	return nil
}

// loadCachedCert loads a previously obtained certificate if it is still valid.
func (m *ACMEManager) loadCachedCert() error {
	certPEM, err := os.ReadFile(filepath.Join(m.cacheDir, ipCertFile))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.cacheDir, ipKeyFile))
	if err != nil {
		return err
	}
	if err := m.setCert(certPEM, keyPEM); err != nil {
		return err
	}
	// Treat an already-expired (or about-to-expire) cached cert as unusable so
	// Start obtains a fresh one instead.
	if time.Now().After(m.notAfter().Add(-time.Hour)) {
		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
		return errors.New("cached certificate expired")
	}
	return nil
}

func (m *ACMEManager) newClient() (*lego.Client, error) {
	key, err := m.loadOrCreateAccountKey()
	if err != nil {
		return nil, err
	}
	user := &acmeUser{email: m.email, key: key}
	cfg := lego.NewConfig(user)
	cfg.CADirURL = m.directory
	cfg.Certificate.KeyType = certcrypto.EC256
	// Let's Encrypt rejects an IP address in the CSR Common Name ("badCSR"); for
	// IP certificates the address must appear only in the SAN. Disabling the CN
	// leaves it empty so the IP is SAN-only.
	cfg.Certificate.DisableCommonName = true
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	// Idempotent: with an existing account key this returns the existing account.
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, err
	}
	user.registration = reg
	return client, nil
}

func (m *ACMEManager) loadOrCreateAccountKey() (crypto.PrivateKey, error) {
	path := filepath.Join(m.cacheDir, accountKeyFile)
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("acme account key %q: not PEM", path)
		}
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err := os.MkdirAll(m.cacheDir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyPEM, err := pemKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFile(path, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// acmeUser implements lego's registration.User.
type acmeUser struct {
	email        string
	registration *registration.Resource
	key          crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }
