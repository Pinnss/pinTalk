package tlscert

import (
	"bytes"
	"crypto/x509"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfSignedDoesNotPersistCAKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateSelfSigned(dir, nil); err != nil {
		t.Fatal(err)
	}
	// The CA signing key is a device-trusted anchor — it must never hit disk.
	if _, err := os.Stat(filepath.Join(dir, "selfsigned-ca.key")); !os.IsNotExist(err) {
		t.Errorf("selfsigned-ca.key should not exist; stat err = %v", err)
	}
	// The downloadable CA cert and the leaf pair must exist.
	for _, f := range []string{"selfsigned-ca.crt", "selfsigned-leaf.crt", "selfsigned-leaf.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}
}

func TestSelfSignedGeneratesValidChain(t *testing.T) {
	dir := t.TempDir()
	ss, err := LoadOrCreateSelfSigned(dir, []string{"call.example.test", "203.0.113.7"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// CA PEM must parse.
	block := ss.CACertPEM()
	if len(block) == 0 {
		t.Fatal("empty CA PEM")
	}

	leaf := ss.TLSCertificate().Leaf
	if leaf == nil {
		t.Fatal("nil leaf")
	}

	// Leaf must cover localhost, loopback, and the caller-supplied extras.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(block) {
		t.Fatal("CA not appended to pool")
	}
	for _, name := range []string{"localhost", "call.example.test"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("leaf does not verify for %q against local CA: %v", name, err)
		}
	}

	hostsOK, ipsOK := desiredSANs([]string{"call.example.test", "203.0.113.7"})
	if !covers(leaf, hostsOK, ipsOK) {
		t.Errorf("leaf does not cover its own desired SANs")
	}
}

func TestSelfSignedCachedThenRegenerated(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateSelfSigned(dir, []string{"a.example.test"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same inputs → cached leaf reused (identical DER).
	cached, err := LoadOrCreateSelfSigned(dir, []string{"a.example.test"})
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if !bytes.Equal(first.TLSCertificate().Certificate[0], cached.TLSCertificate().Certificate[0]) {
		t.Error("expected cached leaf to be reused for identical inputs")
	}

	// New host not covered by the cached leaf → regenerate.
	regen, err := LoadOrCreateSelfSigned(dir, []string{"b.example.test"})
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	if bytes.Equal(first.TLSCertificate().Certificate[0], regen.TLSCertificate().Certificate[0]) {
		t.Error("expected leaf to be regenerated when a new SAN is requested")
	}
}

func TestACMEHTTPHandlerServesChallenge(t *testing.T) {
	mgr := NewACMEManager("203.0.113.7", "", t.TempDir(), log.New(bytes.NewBuffer(nil), "", 0))
	const token, keyAuth = "tok123", "tok123.keyauthvalue"
	if err := mgr.Present("203.0.113.7", token, keyAuth); err != nil {
		t.Fatalf("present: %v", err)
	}

	fallbackHit := false
	h := mgr.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHit = true
		w.WriteHeader(http.StatusTeapot)
	}))

	// Known token → keyAuth body.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, acmeChallengePrefix+token, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != keyAuth {
		t.Errorf("challenge: code=%d body=%q, want 200 %q", rec.Code, rec.Body.String(), keyAuth)
	}

	// Unknown token → 404, fallback not involved.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, acmeChallengePrefix+"nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: code=%d, want 404", rec.Code)
	}

	// Non-challenge path → fallback.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app", nil))
	if !fallbackHit || rec.Code != http.StatusTeapot {
		t.Errorf("fallback not invoked for /app (hit=%v code=%d)", fallbackHit, rec.Code)
	}

	// After CleanUp the token is gone.
	if err := mgr.CleanUp("203.0.113.7", token, keyAuth); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, acmeChallengePrefix+token, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("after cleanup: code=%d, want 404", rec.Code)
	}
}

func TestACMEAccountKeyPersists(t *testing.T) {
	mgr := NewACMEManager("203.0.113.7", "", t.TempDir(), log.New(bytes.NewBuffer(nil), "", 0))
	k1, err := mgr.loadOrCreateAccountKey()
	if err != nil {
		t.Fatalf("first key: %v", err)
	}
	k2, err := mgr.loadOrCreateAccountKey()
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	b1, _ := x509.MarshalPKCS8PrivateKey(k1)
	b2, _ := x509.MarshalPKCS8PrivateKey(k2)
	if !bytes.Equal(b1, b2) {
		t.Error("account key not persisted/reused across calls")
	}
}
