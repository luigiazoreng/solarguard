package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/labels"
	"github.com/luigiazoreng/solarguard/internal/mcpserver"
	"github.com/luigiazoreng/solarguard/internal/store"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*.html
var templatesFS embed.FS

type Config struct {
	MCPToken     string
	TrustProxy   bool // the port is reachable only through a reverse proxy
	SignupClosed bool // only existing accounts can enter
	Store        *store.Store
	Tokens       *ingest.Manager // nil: accounts cannot add provider tokens
	Logger       *slog.Logger
	Now          func() time.Time
}

type Server struct {
	cfg         Config
	store       *store.Store
	sm          *SessionManager
	loginLimit  *attemptLimiter
	signupLimit *attemptLimiter
	tokenLimit  *attemptLimiter
	templates   map[string]*template.Template
	mux         *http.ServeMux
	handler     http.Handler
	mcpHandler  http.Handler
}

type contextKey string

const sessionContextKey contextKey = "session"

// standalonePages render without the layout, because they have no session.
var standalonePages = []string{"login", "signup"}

func NewServer(cfg Config) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	s := &Server{
		cfg:         cfg,
		store:       cfg.Store,
		sm:          NewSessionManager(cfg.Now),
		loginLimit:  newAttemptLimiter(5, 15*time.Minute, cfg.Now),
		signupLimit: newAttemptLimiter(5, time.Hour, cfg.Now),
		tokenLimit:  newAttemptLimiter(5, 15*time.Minute, cfg.Now),
		templates:   make(map[string]*template.Template),
		mux:         http.NewServeMux(),
	}

	// Initialize MCP HTTPHandler if token is provided
	if strings.TrimSpace(cfg.MCPToken) != "" {
		mcpH, err := mcpserver.HTTPHandler(cfg.Store, cfg.MCPToken)
		if err != nil {
			return nil, fmt.Errorf("iniciar handler MCP: %w", err)
		}
		s.mcpHandler = mcpH
	}

	// Parse HTML templates per page to avoid namespace collisions on "content"
	funcMap := template.FuncMap{
		"hasCorruptedName":   func(name string) bool { return strings.ContainsRune(name, '\uFFFD') },
		"formatFloat":        FormatFloat,
		"formatInt":          FormatInt,
		"formatPercent":      FormatPercent,
		"formatDate":         FormatDate,
		"formatTime":         FormatTimeInZone,
		"renderMarkdown":     RenderMarkdown,
		"labelsRule":         labels.Rule,
		"labelsReason":       labels.Reason,
		"labelsSeverity":     labels.Severity,
		"labelsStatus":       labels.Status,
		"labelsPipelineStep": labels.PipelineStep,
		"labelsParam":        labels.Param,
		"printFloat":         func(f float64) string { return fmt.Sprintf("%v", f) },
		"add":                func(a, b int) int { return a + b },
		"sub":                func(a, b int) int { return a - b },
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
		"account",
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

	for _, page := range standalonePages {
		tmpl, err := template.New(page+".html").Funcs(funcMap).ParseFS(templatesFS, "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, err)
		}
		s.templates[page] = tmpl
	}

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
	if slices.Contains(standalonePages, page) {
		templateName = page + ".html"
	}
	return tmpl.ExecuteTemplate(w, templateName, data)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
		if s.mcpHandler == nil {
			s.cfg.Logger.Info("MCP endpoint accessed but disabled (SOLARGUARD_MCP_TOKEN not set)")
			http.Error(w, "MCP disabled", http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		s.mcpHandler.ServeHTTP(w, r)
		return
	}
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Handler() http.Handler {
	return s
}

func (s *Server) ActiveSessionCount() int {
	return s.sm.ActiveSessionCount()
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
	s.mux.HandleFunc("GET /signup", s.handleSignupPage)
	s.mux.HandleFunc("POST /signup", s.handleSignupSubmit)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	s.mux.HandleFunc("GET /account", s.handleAccount)
	s.mux.HandleFunc("POST /account/token", s.handleAccountTokenSave)
	s.mux.HandleFunc("POST /account/token/remove", s.handleAccountTokenRemove)

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

	// CSRF and Origin check on POST. It reads the session from the request
	// context, so it must run inside the auth middleware.
	h = s.csrfMiddleware(h)

	// Auth & session middleware
	h = s.authMiddleware(h)

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

		// Public assets and account entry points
		if strings.HasPrefix(path, "/static/") || isAccountEntry(path) {
			next.ServeHTTP(w, r)
			return
		}

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
			if !VerifyOrigin(r) {
				http.Error(w, "Origem da requisição inválida", http.StatusForbidden)
				return
			}
			// Before the login there is no session to hold a CSRF token.
			if !isAccountEntry(r.URL.Path) {
				// Session & CSRF verification
				sess, _ := r.Context().Value(sessionContextKey).(*Session)

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

func (s *Server) baseTemplateData(r *http.Request, title, activeNav string) map[string]any {
	var csrfToken, email string
	if sess, ok := r.Context().Value(sessionContextKey).(*Session); ok && sess != nil {
		csrfToken = sess.CSRFToken
		email = sess.Email
	}

	return map[string]any{
		"Title":     title,
		"ActiveNav": activeNav,
		"CSRFToken": csrfToken,
		"UserEmail": email,
	}
}
