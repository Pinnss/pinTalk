package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/Pinnss/pinTalk/internal/auth"
	"github.com/Pinnss/pinTalk/internal/config"
	"github.com/Pinnss/pinTalk/internal/server"
	"github.com/Pinnss/pinTalk/internal/tlscert"
	"github.com/Pinnss/pinTalk/internal/turn"
	"github.com/Pinnss/pinTalk/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := cmdServe(os.Args[2:]); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "init":
		if err := cmdInit(os.Args[2:]); err != nil {
			log.Fatalf("init: %v", err)
		}
	case "hash":
		if err := cmdHash(os.Args[2:]); err != nil {
			log.Fatalf("hash: %v", err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: pintalk <command>")
	fmt.Fprintln(os.Stderr, "  init [--config path]            interactive setup wizard: writes config.yaml")
	fmt.Fprintln(os.Stderr, "  serve [--dev] [--config path]   start the server")
	fmt.Fprintln(os.Stderr, "  hash <password>                 print bcrypt hash for config.yaml")
}

func cmdHash(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pintalk hash <password>")
	}
	h, err := auth.HashPassword(args[0])
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dev := fs.Bool("dev", false, "run on dev_port over plain HTTP (no TLS, no TURN required)")
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("[pintalk] ")

	srv, err := server.New(cfg, web.FS, logger, *dev)
	if err != nil {
		return err
	}
	handler := srv.Routes()

	// TURN (skip in dev unless explicitly enabled; binding 3478 may need privileges)
	var turnSrv *turn.Server
	if cfg.TURN.Enabled && !*dev {
		turnSrv, err = turn.Start(cfg.TURN, logger)
		if err != nil {
			return fmt.Errorf("start TURN: %w", err)
		}
	}
	defer func() {
		if turnSrv != nil {
			_ = turnSrv.Close()
		}
	}()

	ctx, cancel := signalContext()
	defer cancel()

	if *dev {
		return serveDev(ctx, cancel, cfg, handler, logger)
	}
	switch cfg.TLS.Mode {
	case config.ModeLetsEncryptDomain:
		return serveAutocert(ctx, cancel, cfg, handler, logger)
	case config.ModeSelfSigned:
		return serveSelfSigned(ctx, cancel, cfg, handler, srv, logger)
	case config.ModeLetsEncryptIP:
		return serveLetsEncryptIP(ctx, cancel, cfg, handler, srv, logger)
	default:
		return fmt.Errorf("unknown tls mode %q", cfg.TLS.Mode)
	}
}

// serveDev serves plain HTTP on dev_port (no TLS) for local development.
func serveDev(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, handler http.Handler, logger *log.Logger) error {
	addr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.DevPort)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Printf("dev mode: listening on http://localhost%s (no TLS)", addr)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("http: %v", err)
			cancel()
		}
	}()
	<-ctx.Done()
	return shutdown(logger, httpSrv)
}

// serveAutocert serves HTTPS with automatic Let's Encrypt certificates for
// server.domain (ACME HTTP-01 on :80, redirect plain HTTP to HTTPS).
func serveAutocert(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, handler http.Handler, logger *log.Logger) error {
	if err := os.MkdirAll(cfg.TLS.CertCache, 0o700); err != nil {
		return fmt.Errorf("mkdir cert cache: %w", err)
	}
	manager := &autocert.Manager{
		Cache:      autocert.DirCache(cfg.TLS.CertCache),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.Server.Domain),
		Email:      cfg.TLS.Email,
	}

	httpsAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPSPort)
	httpsSrv := &http.Server{
		Addr:              httpsAddr,
		Handler:           handler,
		TLSConfig:         &tls.Config{GetCertificate: manager.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}

	httpAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPPort)
	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           manager.HTTPHandler(http.HandlerFunc(redirectToHTTPS(cfg.Server.Domain, cfg.Server.HTTPSPort))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("https listening on %s (letsencrypt domain=%s)", httpsAddr, cfg.Server.Domain)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("https: %v", err)
			cancel()
		}
	}()
	go func() {
		logger.Printf("http listening on %s (ACME + redirect)", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("http: %v", err)
			cancel()
		}
	}()

	<-ctx.Done()
	return shutdown(logger, httpsSrv, httpSrv)
}

// serveSelfSigned serves HTTPS with a locally-generated self-signed certificate.
// No domain required — works by IP / on a LAN. The plain-HTTP listener offers
// the CA for download and redirects everything else to HTTPS; because :80 is
// only a convenience here, failing to bind it is non-fatal.
func serveSelfSigned(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, handler http.Handler, srv *server.Server, logger *log.Logger) error {
	ss, err := tlscert.LoadOrCreateSelfSigned(cfg.TLS.CertCache, selfSignedHosts(cfg))
	if err != nil {
		return fmt.Errorf("self-signed cert: %w", err)
	}
	srv.SetCACertPEM(ss.CACertPEM())

	httpsAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPSPort)
	httpsSrv := &http.Server{
		Addr:    httpsAddr,
		Handler: handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{*ss.TLSCertificate()},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpAddr := cfg.Server.ListenIP + ":" + strconv.Itoa(cfg.Server.HTTPPort)
	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           caDownloadOrRedirect(ss.CACertPEM(), cfg.Server.HTTPSPort),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("https listening on %s (self-signed TLS)", httpsAddr)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("https: %v", err)
			cancel()
		}
	}()
	go func() {
		logger.Printf("http listening on %s (CA download + redirect to HTTPS)", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Non-fatal: HTTPS still works; users just click through the warning.
			logger.Printf("http (:%d) unavailable, CA download/redirect disabled: %v", cfg.Server.HTTPPort, err)
		}
	}()

	<-ctx.Done()
	return shutdown(logger, httpsSrv, httpSrv)
}

// selfSignedHosts collects the hostnames / IPs to add to the self-signed leaf's
// SANs, on top of localhost and the auto-detected local interface IPs.
func selfSignedHosts(cfg *config.Config) []string {
	var extra []string
	add := func(v string) {
		if v != "" {
			extra = append(extra, v)
		}
	}
	add(cfg.Server.Domain)
	add(cfg.TLS.PublicIP)
	add(cfg.TURN.ExternalIP)
	add(cfg.Server.ListenIP)
	return extra
}

func redirectToHTTPS(domain string, httpsPort int) func(http.ResponseWriter, *http.Request) {
	target := "https://" + domain
	if httpsPort != 443 {
		target += ":" + strconv.Itoa(httpsPort)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
	}
}

// caDownloadOrRedirect serves the local CA at /pintalk-ca.crt (over plain HTTP,
// so it can be downloaded before the cert is trusted) and redirects everything
// else to HTTPS on the same host the client used.
func caDownloadOrRedirect(caPEM []byte, httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pintalk-ca.crt" {
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			w.Header().Set("Content-Disposition", `attachment; filename="pintalk-ca.crt"`)
			_, _ = w.Write(caPEM)
			return
		}
		host := hostOnly(r.Host)
		target := "https://" + host
		if httpsPort != 443 {
			target = "https://" + net.JoinHostPort(host, strconv.Itoa(httpsPort))
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// hostOnly strips an optional :port from a Host header value.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// shutdown gracefully stops the given HTTP servers, returning the first error
// (e.g. the 10s drain deadline expiring) so an unclean exit is visible.
func shutdown(logger *log.Logger, servers ...*http.Server) error {
	logger.Printf("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var firstErr error
	for _, s := range servers {
		if s == nil {
			continue
		}
		if err := s.Shutdown(shutCtx); err != nil {
			logger.Printf("shutdown: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
