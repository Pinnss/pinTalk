package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/turn/v3"

	"github.com/Pinnss/pinTalk/internal/config"
)

type Server struct {
	srv  *turn.Server
	udp  net.PacketConn
	tcp  net.Listener
	cfg  config.TURNConfig
	log  *log.Logger
}

// Start launches an embedded TURN server on UDP+TCP listening on cfg.Port.
// Auth uses the standard "ephemeral REST API" scheme: username encodes an expiry
// timestamp and password = base64(HMAC-SHA1(secret, username)).
func Start(cfg config.TURNConfig, logger *log.Logger) (*Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	listenAddr := net.JoinHostPort(cfg.ListenIP, strconv.Itoa(cfg.Port))
	udp, err := net.ListenPacket("udp4", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("turn udp listen: %w", err)
	}
	tcp, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("turn tcp listen: %w", err)
	}

	relayIP := net.ParseIP(cfg.ExternalIP)
	if relayIP == nil {
		// Fall back to listen IP. ICE will only work LAN-local in this case.
		relayIP = net.ParseIP(cfg.ListenIP)
		if relayIP == nil || relayIP.IsUnspecified() {
			relayIP = net.IPv4(127, 0, 0, 1)
		}
	}

	authHandler := func(username, realm string, srcAddr net.Addr) ([]byte, bool) {
		key, ok := credKey(username, cfg.SharedSecret)
		if !ok {
			return nil, false
		}
		// pion/turn expects the long-term auth key = MD5(username:realm:password).
		// REST creds use HMAC-SHA1 password, so we synthesize it here.
		password := base64.StdEncoding.EncodeToString(key)
		return turn.GenerateAuthKey(username, realm, password), true
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm:       cfg.Realm,
		AuthHandler: authHandler,
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: udp,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: relayIP,
					Address:      cfg.ListenIP,
				},
			},
		},
		ListenerConfigs: []turn.ListenerConfig{
			{
				Listener: tcp,
				RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
					RelayAddress: relayIP,
					Address:      cfg.ListenIP,
				},
			},
		},
	})
	if err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		return nil, fmt.Errorf("turn.NewServer: %w", err)
	}
	if logger != nil {
		logger.Printf("TURN listening on %s (udp+tcp), realm=%q, relay=%s", listenAddr, cfg.Realm, relayIP)
	}
	return &Server{srv: srv, udp: udp, tcp: tcp, cfg: cfg, log: logger}, nil
}

func (s *Server) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// EphemeralCredentials returns a username/password pair valid for cfg.CredTTLMinutes.
// Username format per RFC 5766 §5: "<unix-expiry>:<arbitrary>".
func EphemeralCredentials(secret string, ttl time.Duration, suffix string) (username, password string) {
	expiry := time.Now().Add(ttl).Unix()
	if suffix == "" {
		suffix = "p"
	}
	username = fmt.Sprintf("%d:%s", expiry, suffix)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	password = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return
}

// credKey verifies the username has not expired and returns the HMAC-SHA1 key for it.
func credKey(username, secret string) ([]byte, bool) {
	parts := strings.SplitN(username, ":", 2)
	if len(parts) < 1 || parts[0] == "" {
		return nil, false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, false
	}
	if time.Now().Unix() > expiry {
		return nil, false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return mac.Sum(nil), true
}

var _ = errors.New
