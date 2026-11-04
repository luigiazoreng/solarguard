package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/store"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*.html
var templatesFS embed.FS

var (
	ErrInvalidPassword = errors.New("senha incorreta")
	ErrTooManyRequests = errors.New("muitas tentativas inválidas; tente novamente mais tarde")
)

type Config struct {
	Addr          string
	AdminPassword string
	Store         *store.Store
	Logger        *slog.Logger
	Now           func() time.Time
}

type Server struct {
	cfg            Config
	store          *store.Store
	sm             *SessionManager
	templates      map[string]*template.Template
	mux            *http.ServeMux
	handler        http.Handler
	noAuthRequired bool
	testCSRFToken  string
}

type contextKey string

const sessionContextKey contextKey = "session"

func NewServer(cfg Config) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	s := &Server{
		cfg:       cfg,
		store:     cfg.Store,
		sm:        NewSessionManager(cfg.AdminPassword, cfg.Now),
		templates: make(map[string]*template.Template),
		mux:       http.NewServeMux(),
	}

	// Security check: Refuse non-loopback start if password is empty
	if cfg.AdminPassword == "" {
		if !IsLoopback(cfg.Addr) {
			return nil, errors.New("SOLARGUARD_ADMIN_PASSWORD is required for non-loopback addresses")
		}
		s.noAuthRequired = true
	}

	// Parse HTML templates per page to avoid namespace collisions on "content"
	funcMap := template.FuncMap{
		"formatFloat":    FormatFloat,
		"formatInt":      FormatInt,
		"formatPercent":  FormatPercent,
		"formatDate":     FormatDate,
		"formatTime":     FormatTimeInZone,
		"renderMarkdown": RenderMarkdown,
		"add":            func(a, b int) int { return a + b },
		"sub":            func(a, b int) int { return a - b },
		"derefFloat": func(f *float64) float64 {
			if f == nil {
				return 0
			}
			return *f
		},
		"derefBool": func(b *bool) bool {
			if b == nil {
				return false
			}
			return *b
		},
		"derefStr": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"makeTimePtr":   func(t time.Time) *time.Time { return &t },
		"multiplyFloat": func(a, b float64) float64 { return a * b },
		"divideFloat": func(a, b int) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b)
		},
	}

	layoutTmpl, err := template.New("layout.html").Funcs(funcMap).ParseFS(templatesFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout template: %w", err)
	}

	pageFiles := []string{
		"overview",
		"plant",
		"alerts",
		"alert_detail",
		"reports",
		"report_detail",
		"manage",
	}

	for _, page := range pageFiles {
		clone, err := layoutTmpl.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone layout for %s: %w", page, err)
		}
		pageTmpl, err := clone.ParseFS(templatesFS, "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template for %s: %w", page, err)
		}
		s.templates[page] = pageTmpl
	}

	loginTmpl, err := template.New("login.html").Funcs(funcMap).ParseFS(templatesFS, "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
	}
	s.templates["login"] = loginTmpl

	s.routes()
	s.buildHandlerChain()

	return s, nil
}

func (s *Server) render(w http.ResponseWriter, page string, data any) error {
	tmpl, ok := s.templates[page]
	if !ok {
		return fmt.Errorf("template %s not found", page)
	}
	templateName := "layout.html"
	if page == "login" {
		templateName = "login.html"
	}
	return tmpl.ExecuteTemplate(w, templateName, data)
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) NoAuthRequired() bool {
	return s.noAuthRequired
}

func (s *Server) TestCSRFToken() string {
	if s.testCSRFToken == "" {
		sess := s.sm.CreateAnonymousSession()
		s.testCSRFToken = sess.CSRFToken
	}
	return s.testCSRFToken
}

func (s *Server) routes() {
	// Static assets
	subStatic, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(subStatic))
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))

	// Auth routes
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLoginSubmit)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	// App routes
	s.mux.HandleFunc("GET /{$}", s.handleOverview)
	s.mux.HandleFunc("GET /plants/{id}", s.handlePlantDetail)
	s.mux.HandleFunc("GET /api/v1/plants/{id}/series", s.handlePlantSeries)

	s.mux.HandleFunc("GET /alerts", s.handleAlertsList)
	s.mux.HandleFunc("GET /alerts/{id}", s.handleAlertDetail)
	s.mux.HandleFunc("POST /alerts/{id}/ack", s.handleAlertAck)
	s.mux.HandleFunc("POST /alerts/{id}/resolve", s.handleAlertResolve)

	s.mux.HandleFunc("GET /reports", s.handleReportsList)
	s.mux.HandleFunc("GET /reports/{id}", s.handleReportDetail)
	s.mux.HandleFunc("POST /reports/generate", s.handleReportGenerate)
	s.mux.HandleFunc("POST /reports/{id}/finalize", s.handleReportFinalize)

	s.mux.HandleFunc("GET /manage", s.handleManage)
	s.mux.HandleFunc("POST /manage/plants/{id}", s.handleManagePlantUpdate)
	s.mux.HandleFunc("POST /manage/devices/{id}", s.handleManageDeviceUpdate)
	s.mux.HandleFunc("POST /manage/rules", s.handleManageRuleUpdate)
	s.mux.HandleFunc("POST /manage/pipeline/run", s.handleManagePipelineRun)
}

func (s *Server) buildHandlerChain() {
	h := http.Handler(s.mux)

	// Auth & session middleware
	h = s.authMiddleware(h)

	// CSRF and Origin check on POST
	h = s.csrfMiddleware(h)

	// Security Headers
	h = s.securityHeaders(h)

	// Panic recovery
	h = s.panicRecovery(h)

	s.handler = h
}

func (s *Server) panicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.cfg.Logger.Error("panic recovered in HTTP handler", "panic", rec, "path", r.URL.Path)
				http.Error(w, "Erro interno do servidor", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Security-Policy with default-src 'self' and no inline scripts
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:;")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Public assets and login endpoints
		if strings.HasPrefix(path, "/static/") || path == "/login" {
			next.ServeHTTP(w, r)
			return
		}

		if s.noAuthRequired {
			// In no-auth loopback mode, ensure session exists for CSRF protection
			var sess *Session
			cookie, err := r.Cookie("solarguard_session")
			if err == nil {
				sess, _ = s.sm.GetSession(cookie.Value)
			}
			if sess == nil {
				sess = s.sm.CreateAnonymousSession()
				isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
				http.SetCookie(w, &http.Cookie{
					Name:     "solarguard_session",
					Value:    sess.Token,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					Secure:   isSecure,
					MaxAge:   12 * 3600,
				})
			}
			ctx := context.WithValue(r.Context(), sessionContextKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Authenticated mode
		cookie, err := r.Cookie("solarguard_session")
		if err != nil {
			s.redirectOrUnauthorized(w, r)
			return
		}

		sess, ok := s.sm.GetSession(cookie.Value)
		if !ok {
			s.redirectOrUnauthorized(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), sessionContextKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) redirectOrUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Skip CSRF check on login submit itself
			if r.URL.Path != "/login" {
				// Origin verification
				if !VerifyOrigin(r) {
					http.Error(w, "Origem da requisição inválida", http.StatusForbidden)
					return
				}

				// Session & CSRF verification
				sess, _ := r.Context().Value(sessionContextKey).(*Session)
				if sess == nil && s.testCSRFToken != "" {
					sess = &Session{CSRFToken: s.testCSRFToken}
				}

				token := r.PostFormValue("csrf_token")
				if token == "" {
					token = r.Header.Get("HX-CSRF-Token")
				}
				if token == "" {
					token = r.Header.Get("X-CSRF-Token")
				}

				if !VerifyCSRF(sess, token) {
					http.Error(w, "Token CSRF inválido ou ausente", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.noAuthRequired {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	_ = s.render(w, "login", map[string]any{
		"Error": "",
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.noAuthRequired {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	password := r.PostFormValue("password")
	remoteIP := r.RemoteAddr

	sess, err := s.sm.Authenticate(password, remoteIP)
	if errors.Is(err, ErrTooManyRequests) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = s.render(w, "login", map[string]any{
			"Error": "Muitas tentativas incorretas. Tente novamente em 15 minutos.",
		})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = s.render(w, "login", map[string]any{
			"Error": "Senha incorreta.",
		})
		return
	}

	isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "solarguard_session",
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure,
		MaxAge:   12 * 3600,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("solarguard_session"); err == nil {
		s.sm.RevokeSession(cookie.Value)
	}

	isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "solarguard_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) baseTemplateData(r *http.Request, title, activeNav string) map[string]any {
	var csrfToken string
	if sess, ok := r.Context().Value(sessionContextKey).(*Session); ok && sess != nil {
		csrfToken = sess.CSRFToken
	}

	return map[string]any{
		"Title":              title,
		"ActiveNav":          activeNav,
		"CSRFToken":          csrfToken,
		"ShowLoopbackBanner": s.noAuthRequired,
		"NoAuth":             s.noAuthRequired,
	}
}
