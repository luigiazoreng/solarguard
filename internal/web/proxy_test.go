package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/web"
)

func loginFrom(t *testing.T, baseURL, forwardedFor, password string) int {
	t.Helper()
	form := url.Values{"password": {password}}.Encode()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/login", strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Behind a reverse proxy every connection comes from loopback. A stranger's
// wrong passwords must not lock the owner out of the panel.
func TestLoginLockoutIsPerClientBehindProxy(t *testing.T) {
	st := newWebTestStore(t)
	ts := startServer(t, st, "senha-certa", time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))

	for i := 0; i < 5; i++ {
		if got := loginFrom(t, ts.URL, "203.0.113.1", "errada"); got != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: status %d, want 401", i+1, got)
		}
	}
	if got := loginFrom(t, ts.URL, "203.0.113.1", "senha-certa"); got != http.StatusTooManyRequests {
		t.Errorf("locked client with the right password: status %d, want 429", got)
	}
	if got := loginFrom(t, ts.URL, "203.0.113.2", "senha-certa"); got != http.StatusSeeOther {
		t.Errorf("another client: status %d, want 303", got)
	}
}

// The proxy appends the address it saw, so only the last entry can be trusted.
// Changing the earlier entries must not escape a lockout.
func TestLoginLockoutCannotBeEscapedByForgingForwardedFor(t *testing.T) {
	st := newWebTestStore(t)
	ts := startServer(t, st, "senha-certa", time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))

	for i := 0; i < 5; i++ {
		loginFrom(t, ts.URL, "203.0.113.1", "errada")
	}
	if got := loginFrom(t, ts.URL, "198.51.100.9, 203.0.113.1", "senha-certa"); got != http.StatusTooManyRequests {
		t.Errorf("forged first entry: status %d, want 429", got)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, remote, forwarded string
		trustProxy              bool
		want                    string
	}{
		{"direct connection", "198.51.100.1:4000", "", false, "198.51.100.1"},
		{"untrusted peer cannot claim another address", "172.18.0.5:4000", "203.0.113.1", false, "172.18.0.5"},
		{"proxy in another container", "172.18.0.5:4000", "203.0.113.1", true, "203.0.113.1"},
		{"proxy on loopback", "127.0.0.1:4000", "203.0.113.1", false, "203.0.113.1"},
		{"proxy on ipv6 loopback", "[::1]:4000", "2001:db8::1", false, "2001:db8::1"},
		{"last entry wins", "172.18.0.5:4000", "198.51.100.9, 203.0.113.1", true, "203.0.113.1"},
		{"trusted proxy without header", "172.18.0.5:4000", "", true, "172.18.0.5"},
		{"invalid header", "172.18.0.5:4000", "nao-e-ip", true, "172.18.0.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/login", nil)
			r.RemoteAddr = c.remote
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-For", c.forwarded)
			}
			if got := web.ClientIP(r, c.trustProxy); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}
