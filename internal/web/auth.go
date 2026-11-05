package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Session struct {
	Token     string
	CSRFToken string
	UserID    int64
	Email     string
	ExpiresAt time.Time
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	now      func() time.Time
}

func NewSessionManager(now func() time.Time) *SessionManager {
	if now == nil {
		now = time.Now
	}
	return &SessionManager{
		sessions: make(map[string]*Session),
		now:      now,
	}
}

func generateRandomToken(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateSession starts a session for an account that already proved who it is.
func (sm *SessionManager) CreateSession(userID int64, email string) (*Session, error) {
	token, err := generateRandomToken(32)
	if err != nil {
		return nil, err
	}
	csrfToken, err := generateRandomToken(32)
	if err != nil {
		return nil, err
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := sm.now()
	for k, v := range sm.sessions {
		if now.After(v.ExpiresAt) {
			delete(sm.sessions, k)
		}
	}
	sess := &Session{
		Token:     token,
		CSRFToken: csrfToken,
		UserID:    userID,
		Email:     email,
		ExpiresAt: now.Add(12 * time.Hour),
	}
	sm.sessions[token] = sess
	return sess, nil
}

// attemptLimiter blocks a key for a while after too many attempts.
type attemptLimiter struct {
	mu       sync.Mutex
	max      int
	lockout  time.Duration
	now      func() time.Time
	attempts map[string]*attemptCount
}

type attemptCount struct {
	count int
	until time.Time
}

func newAttemptLimiter(max int, lockout time.Duration, now func() time.Time) *attemptLimiter {
	return &attemptLimiter{max: max, lockout: lockout, now: now, attempts: make(map[string]*attemptCount)}
}

// Blocked reports whether the key used up its attempts.
func (l *attemptLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[key]
	if !ok {
		return false
	}
	if !l.now().Before(a.until) {
		delete(l.attempts, key)
		return false
	}
	return a.count >= l.max
}

// Record counts one attempt. The block runs from the latest attempt.
func (l *attemptLimiter) Record(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, a := range l.attempts {
		if !now.Before(a.until) {
			delete(l.attempts, k)
		}
	}
	a, ok := l.attempts[key]
	if !ok {
		a = &attemptCount{}
		l.attempts[key] = a
	}
	a.count++
	a.until = now.Add(l.lockout)
}

// Clear forgets the attempts of a key.
func (l *attemptLimiter) Clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (sm *SessionManager) GetSession(token string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sess, ok := sm.sessions[token]
	if !ok {
		return nil, false
	}
	if sm.now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

func (sm *SessionManager) RevokeSession(token string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, token)
}

func (sm *SessionManager) ActiveSessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// ClientIP returns the address used to count failed logins. X-Forwarded-For is
// trusted only from a loopback peer, or from any peer when trustProxy says the
// port is reachable only through the reverse proxy. Only its last entry counts,
// because that is the one the proxy appended.
func ClientIP(r *http.Request, trustProxy bool) string {
	peer := extractIP(r.RemoteAddr)
	if !trustProxy && !IsLoopback(peer) {
		return peer
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer
	}
	entries := strings.Split(forwarded[len(forwarded)-1], ",")
	last := strings.TrimSpace(entries[len(entries)-1])
	if net.ParseIP(last) == nil {
		return peer
	}
	return last
}

// IsLoopback checks whether addr (or host part) represents a loopback address.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// VerifyCSRF checks if the provided CSRF token matches the session in constant time.
func VerifyCSRF(sess *Session, token string) bool {
	if sess == nil || token == "" || sess.CSRFToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(token)) == 1
}

// VerifyOrigin verifies that the request origin matches the request host.
func VerifyOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		// Origin header might be absent in same-origin forms on some clients
		return true
	}

	clean := strings.TrimPrefix(origin, "https://")
	clean = strings.TrimPrefix(clean, "http://")
	slash := strings.Index(clean, "/")
	if slash != -1 {
		clean = clean[:slash]
	}
	return strings.EqualFold(clean, r.Host)
}
