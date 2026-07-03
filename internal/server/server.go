package server

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Pinnss/pinTalk/internal/auth"
	"github.com/Pinnss/pinTalk/internal/config"
	"github.com/Pinnss/pinTalk/internal/room"
	"github.com/Pinnss/pinTalk/internal/signal"
	"github.com/Pinnss/pinTalk/internal/turn"
)

type Server struct {
	cfg       *config.Config
	auth      *auth.Store
	rooms     *room.Registry
	signal    *signal.Handler
	logger    *log.Logger
	web       embed.FS
	tpls      *template.Template
	devMode   bool
	caCertPEM []byte // local CA offered at /pintalk-ca.crt (self-signed mode); nil otherwise
}

// SetCACertPEM registers the local CA certificate served at /pintalk-ca.crt so
// users can install it once and avoid the browser warning in self-signed mode.
func (s *Server) SetCACertPEM(pem []byte) { s.caCertPEM = pem }

func New(cfg *config.Config, webFS embed.FS, logger *log.Logger, devMode bool) (*Server, error) {
	tpls, err := template.ParseFS(webFS, "*.html")
	if err != nil {
		return nil, err
	}

	authStore := auth.NewStore(cfg.Hosts)
	rooms := room.NewRegistry()
	sig := &signal.Handler{
		Auth:    authStore,
		Rooms:   rooms,
		Logger:  logger,
		DevMode: devMode,
	}

	return &Server{
		cfg:     cfg,
		auth:    authStore,
		rooms:   rooms,
		signal:  sig,
		logger:  logger,
		web:     webFS,
		tpls:    tpls,
		devMode: devMode,
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// static .css/.js (everything else is gated through templates)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(s.web)))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /app", s.requireAuth(s.handleApp))
	mux.HandleFunc("GET /api/rooms", s.requireAuth(s.handleListRooms))
	mux.HandleFunc("POST /api/rooms", s.requireAuth(s.handleCreateRoom))
	mux.HandleFunc("DELETE /api/rooms/{id}", s.requireAuth(s.handleDeleteRoom))
	mux.HandleFunc("GET /c/{id}", s.handleCallPage)
	mux.HandleFunc("GET /api/ice-config", s.handleICEConfig)
	mux.HandleFunc("GET /pintalk-ca.crt", s.handleCACert)
	mux.Handle("GET /ws", s.signal)

	return s.logMiddleware(mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if sess := s.auth.FromRequest(r); sess != nil {
		http.Redirect(w, r, "/app", http.StatusFound)
		return
	}
	s.render(w, "login.html", map[string]any{"Error": ""})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Username = r.PostFormValue("username")
		req.Password = r.PostFormValue("password")
	}

	sess, err := s.auth.Login(req.Username, req.Password)
	if err != nil {
		if strings.HasPrefix(ct, "application/json") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad credentials"})
		} else {
			s.render(w, "login.html", map[string]any{"Error": "Неверный логин или пароль"})
		}
		return
	}
	auth.SetCookie(w, sess, !s.devMode)
	if strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return
	}
	http.Redirect(w, r, "/app", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.auth.Logout(c.Value)
	}
	auth.ClearCookie(w, !s.devMode)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	s.render(w, "app.html", map[string]any{"Username": sess.Username})
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	rm := s.rooms.Create(sess.Username)
	writeJSON(w, http.StatusOK, map[string]string{
		"id":  rm.ID,
		"url": "/c/" + rm.ID,
	})
}

func (s *Server) handleListRooms(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	list := s.rooms.ListByHost(sess.Username)
	out := make([]map[string]any, 0, len(list))
	for _, rm := range list {
		out = append(out, map[string]any{
			"id":      rm.ID,
			"url":     "/c/" + rm.ID,
			"created": rm.Created.Unix(),
			"peers":   rm.PeerCount(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id := r.PathValue("id")
	rm := s.rooms.Get(id)
	if rm == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if rm.HostUsername != sess.Username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.rooms.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCallPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rm := s.rooms.Get(id)
	if rm == nil {
		http.Error(w, "Звонок не найден или закончился", http.StatusNotFound)
		return
	}
	sess := s.auth.FromRequest(r)
	isHost := sess != nil && sess.Username == rm.HostUsername
	s.render(w, "call.html", map[string]any{
		"RoomID": rm.ID,
		"IsHost": isHost,
		"Host":   rm.HostUsername,
	})
}

func (s *Server) handleICEConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.TURN
	servers := []map[string]any{
		{"urls": []string{"stun:stun.l.google.com:19302"}},
	}
	if cfg.Enabled {
		username, password := turn.EphemeralCredentials(
			cfg.SharedSecret,
			time.Duration(cfg.CredTTLMinutes)*time.Minute,
			"p",
		)
		// Where clients should reach the TURN server: the configured external
		// IP, else the domain, else whatever host the client used to reach us
		// (covers IP/LAN modes that have no domain).
		host := cfg.ExternalIP
		if host == "" {
			host = s.cfg.Server.Domain
		}
		if host == "" {
			host = hostOnly(r.Host)
		}
		port := cfg.Port
		urls := []string{
			"turn:" + host + ":" + itoa(port) + "?transport=udp",
			"turn:" + host + ":" + itoa(port) + "?transport=tcp",
		}
		servers = append(servers, map[string]any{
			"urls":       urls,
			"username":   username,
			"credential": password,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"iceServers": servers})
}

func (s *Server) handleCACert(w http.ResponseWriter, r *http.Request) {
	if len(s.caCertPEM) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="pintalk-ca.crt"`)
	_, _ = w.Write(s.caCertPEM)
}

// --- helpers ---

// hostOnly strips an optional :port from a Host header value ("host:port" or a
// bare "host"), returning just the host.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

type authedHandler func(http.ResponseWriter, *http.Request, *auth.Session)

func (s *Server) requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.auth.FromRequest(r)
		if sess == nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpls.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Printf("template %s: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// Don't log noisy /static and ws frames
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/ws" {
			return
		}
		s.logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

var _ = errors.New
