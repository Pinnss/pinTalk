package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
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

	ctx, cancel := signalContext()
	defer cancel()

	if *dev {
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
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = httpSrv.Shutdown(shutCtx)
		if turnSrv != nil {
			_ = turnSrv.Close()
		}
		return nil
	}

	// Production: autocert TLS on HTTPSPort, ACME HTTP-01 on HTTPPort, plain HTTP redirects.
	if err := os.MkdirAll(cfg.Server.CertCache, 0o700); err != nil {
		return fmt.Errorf("mkdir cert cache: %w", err)
	}
	manager := &autocert.Manager{
		Cache:      autocert.DirCache(cfg.Server.CertCache),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.Server.Domain),
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
		logger.Printf("https listening on %s (domain=%s)", httpsAddr, cfg.Server.Domain)
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
	logger.Printf("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = httpsSrv.Shutdown(shutCtx)
	_ = httpSrv.Shutdown(shutCtx)
	if turnSrv != nil {
		_ = turnSrv.Close()
	}
	return nil
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
