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
	ExpiresAt time.Time
}

type failedAttempt struct {
	count        int
	lastFailure  time.Time
	lockoutUntil time.Time
}

type SessionManager struct {
	mu            sync.RWMutex
	sessions      map[string]*Session
	failures      map[string]*failedAttempt
	adminPassword string
	now           func() time.Time
}

func NewSessionManager(adminPassword string, now func() time.Time) *SessionManager {
	if now == nil {
		now = time.Now
	}
	return &SessionManager{
		sessions:      make(map[string]*Session),
		failures:      make(map[string]*failedAttempt),
		adminPassword: adminPassword,
		now:           now,
	}
}

func generateRandomToken(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Authenticate verifies password against adminPassword in constant time.
// It enforces a per-IP delay and lockout after 5 consecutive failures.
func (sm *SessionManager) Authenticate(password, remoteIP string) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := sm.now()

	// Check lockout
	ipKey := extractIP(remoteIP)
	attempt, exists := sm.failures[ipKey]
	if exists && attempt.count >= 5 {
		if now.Before(attempt.lockoutUntil) {
			return nil, ErrTooManyRequests
		}
		// Lockout expired, reset
		delete(sm.failures, ipKey)
	}

	// Constant-time compare
	passMatch := subtle.ConstantTimeCompare([]byte(password), []byte(sm.adminPassword)) == 1
	if !passMatch {
		if !exists {
			attempt = &failedAttempt{}
			sm.failures[ipKey] = attempt
		}
		attempt.count++
		attempt.lastFailure = now
		if attempt.count >= 5 {
			attempt.lockoutUntil = now.Add(15 * time.Minute)
		}
		// Simple delay to slow brute force
		time.Sleep(50 * time.Millisecond)
		return nil, ErrInvalidPassword
	}

	// Success: clear failures
	delete(sm.failures, ipKey)

	token, _ := generateRandomToken(32)
	csrfToken, _ := generateRandomToken(32)
	sess := &Session{
		Token:     token,
		CSRFToken: csrfToken,
		ExpiresAt: now.Add(12 * time.Hour),
	}
	sm.sessions[token] = sess
	return sess, nil
}

// CreateAnonymousSession creates a session for loopback mode without password.
func (sm *SessionManager) CreateAnonymousSession() *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	token, _ := generateRandomToken(32)
	csrfToken, _ := generateRandomToken(32)
	sess := &Session{
		Token:     token,
		CSRFToken: csrfToken,
		ExpiresAt: sm.now().Add(12 * time.Hour),
	}
	sm.sessions[token] = sess
	return sess
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

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
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

	// Compare host portion
	clean := strings.TrimPrefix(origin, "https://")
	clean = strings.TrimPrefix(clean, "http://")
	slash := strings.Index(clean, "/")
	if slash != -1 {
		clean = clean[:slash]
	}
	return strings.EqualFold(clean, r.Host)
}
