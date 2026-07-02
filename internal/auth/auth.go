package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Pinnss/pinTalk/internal/config"
)

const (
	CookieName     = "pintalk_session"
	SessionTTL     = 7 * 24 * time.Hour
	gcInterval     = 1 * time.Hour
	tokenByteLen   = 32
)

var (
	ErrBadCredentials = errors.New("bad credentials")
	ErrNoSession      = errors.New("no session")
)

type Session struct {
	Token    string
	Username string
	Created  time.Time
	Expires  time.Time
}

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	accounts map[string]string // username → bcrypt hash
}

func NewStore(hosts []config.HostAccount) *Store {
	accounts := make(map[string]string, len(hosts))
	for _, h := range hosts {
		accounts[h.Username] = h.PasswordHash
	}
	s := &Store{
		sessions: make(map[string]*Session),
		accounts: accounts,
	}
	go s.gcLoop()
	return s
}

func (s *Store) Login(username, password string) (*Session, error) {
	s.mu.RLock()
	hash, ok := s.accounts[username]
	s.mu.RUnlock()
	if !ok {
		// burn one CPU cycle on a dummy compare to limit username enumeration
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$............................................................"), []byte(password))
		return nil, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrBadCredentials
	}
	tok := newToken()
	now := time.Now()
	sess := &Session{
		Token:    tok,
		Username: username,
		Created:  now,
		Expires:  now.Add(SessionTTL),
	}
	s.mu.Lock()
	s.sessions[tok] = sess
	s.mu.Unlock()
	return sess, nil
}

func (s *Store) Logout(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *Store) Get(token string) *Session {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(sess.Expires) {
		s.Logout(token)
		return nil
	}
	return sess
}

func (s *Store) FromRequest(r *http.Request) *Session {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	return s.Get(c.Value)
}

func (s *Store) gcLoop() {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for tok, sess := range s.sessions {
			if now.After(sess.Expires) {
				delete(s.sessions, tok)
			}
		}
		s.mu.Unlock()
	}
}

func newToken() string {
	b := make([]byte, tokenByteLen)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func SetCookie(w http.ResponseWriter, sess *Session, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.Expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// HashPassword returns a bcrypt hash for the given password (used by `pintalk hash` cmd).
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
