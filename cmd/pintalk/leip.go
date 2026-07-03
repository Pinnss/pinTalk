package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Pinnss/pinTalk/internal/config"
	"github.com/Pinnss/pinTalk/internal/server"
	"github.com/Pinnss/pinTalk/internal/tlscert"
)

// serveLetsEncryptIP serves HTTPS with a Let's Encrypt certificate issued for a
// bare public IP (short-lived profile, auto-renewed). ACME HTTP-01 validation
// is served on :80, which also redirects normal traffic to HTTPS.
func serveLetsEncryptIP(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, handler http.Handler, srv *server.Server, logger *log.Logger) error {
	if err := os.MkdirAll(cfg.TLS.CertCache, 0o700); err != nil {
		return fmt.Errorf("mkdir cert cache: %w", err)
	}
	ip := cfg.CertIP()
	mgr := tlscert.NewACMEManager(ip, cfg.TLS.Email, cfg.TLS.CertCache, logger)

	// Bind :80 synchronously and start serving BEFORE obtaining, so the ACME
	// HTTP-01 challenge is guaranteed reachable when Let's Encrypt validates.
	httpAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPPort)
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return fmt.Errorf("listen %s for ACME HTTP-01: %w", httpAddr, err)
	}
	httpSrv := &http.Server{
		Handler:           mgr.HTTPHandler(http.HandlerFunc(redirectToHTTPS(ip, cfg.Server.HTTPSPort))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Printf("http listening on %s (ACME HTTP-01 + redirect)", httpAddr)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("http: %v", err)
			cancel()
		}
	}()

	// Obtain (or load cached) certificate; starts the renewal loop.
	if err := mgr.Start(ctx); err != nil {
		cancel() // stop the :80 goroutine and the signal watcher deterministically
		_ = httpSrv.Close()
		return fmt.Errorf("letsencrypt-ip: %w", err)
	}

	httpsAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPSPort)
	httpsSrv := &http.Server{
		Addr:              httpsAddr,
		Handler:           handler,
		TLSConfig:         &tls.Config{GetCertificate: mgr.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Printf("https listening on %s (letsencrypt IP=%s)", httpsAddr, ip)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("https: %v", err)
			cancel()
		}
	}()

	<-ctx.Done()
	return shutdown(logger, httpsSrv, httpSrv)
}
