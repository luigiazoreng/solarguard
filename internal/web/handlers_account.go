package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
)

const (
	passwordCost = 12
	minPassword  = 10
	maxPassword  = 72 // bcrypt's limit, in bytes
	maxEmail     = 254
	maxToken     = 512
	// The server's write timeout is 15 s, so the provider must answer before it.
	connectTimeout = 12 * time.Second
)

const (
	msgBadLogin  = "E-mail ou senha incorretos."
	msgTooMany   = "Muitas tentativas. Tente novamente em alguns minutos."
	msgBadSignup = "Não foi possível criar a conta. Tente novamente."
)

// unknownUserHash gives a login with an unknown e-mail the same cost as a wrong
// password, so the response time does not reveal which e-mails have accounts.
var unknownUserHash = sync.OnceValue(func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("sem conta para este e-mail"), passwordCost)
	if err != nil {
		panic(err)
	}
	return hash
})

// isAccountEntry reports the routes a visitor without a session can use.
func isAccountEntry(path string) bool {
	return path == "/login" || path == "/signup"
}

// NormalizeEmail returns the form in which an e-mail is stored and compared.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail checks a normalized e-mail.
func ValidateEmail(email string) error {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > maxEmail {
		return errors.New("Informe um e-mail válido.")
	}
	return nil
}

// ValidatePassword checks the password rules.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < minPassword {
		return errors.New("A senha precisa ter pelo menos " + strconv.Itoa(minPassword) + " caracteres.")
	}
	if len(password) > maxPassword {
		return errors.New("A senha pode ter no máximo " + strconv.Itoa(maxPassword) + " bytes.")
	}
	return nil
}

// HashPassword returns the hash stored for a password.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "solarguard_session",
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   maxAge,
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user store.User) error {
	sess, err := s.sm.CreateSession(user.ID, user.Email)
	if err != nil {
		return err
	}
	s.setSessionCookie(w, r, sess.Token, 12*3600)
	return nil
}

func (s *Server) hasSession(r *http.Request) bool {
	cookie, err := r.Cookie("solarguard_session")
	if err != nil {
		return false
	}
	_, ok := s.sm.GetSession(cookie.Value)
	return ok
}

func (s *Server) renderEntry(w http.ResponseWriter, page string, status int, email, errMsg string) {
	w.WriteHeader(status)
	err := s.render(w, page, map[string]any{"Email": email, "Error": errMsg, "MinPassword": minPassword, "SignupOpen": !s.cfg.SignupClosed})
	if err != nil {
		s.cfg.Logger.Error("render account entry", "page", page, "err", err)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.renderEntry(w, "login", http.StatusOK, "", "")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	email := NormalizeEmail(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	clientIP := ClientIP(r, s.cfg.TrustProxy)

	if s.loginLimit.Blocked(clientIP) {
		s.renderEntry(w, "login", http.StatusTooManyRequests, email, msgTooMany)
		return
	}

	user, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.cfg.Logger.Error("login: get user", "err", err)
		s.renderEntry(w, "login", http.StatusInternalServerError, email, msgBadSignup)
		return
	}
	hash := []byte(user.PasswordHash)
	if user.ID == 0 {
		hash = unknownUserHash()
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || user.ID == 0 {
		s.loginLimit.Record(clientIP)
		s.renderEntry(w, "login", http.StatusUnauthorized, email, msgBadLogin)
		return
	}

	s.loginLimit.Clear(clientIP)
	if err := s.startSession(w, r, user); err != nil {
		s.cfg.Logger.Error("login: start session", "err", err)
		s.renderEntry(w, "login", http.StatusInternalServerError, email, msgBadSignup)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSignupPage(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if s.cfg.SignupClosed {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.renderEntry(w, "signup", http.StatusOK, "", "")
}

func (s *Server) handleSignupSubmit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SignupClosed {
		s.renderEntry(w, "login", http.StatusForbidden, "", "Novos cadastros estão desativados.")
		return
	}

	email := NormalizeEmail(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	clientIP := ClientIP(r, s.cfg.TrustProxy)

	if s.signupLimit.Blocked(clientIP) {
		s.renderEntry(w, "signup", http.StatusTooManyRequests, email, msgTooMany)
		return
	}
	if err := ValidateEmail(email); err != nil {
		s.renderEntry(w, "signup", http.StatusUnprocessableEntity, email, err.Error())
		return
	}
	if err := ValidatePassword(password); err != nil {
		s.renderEntry(w, "signup", http.StatusUnprocessableEntity, email, err.Error())
		return
	}
	if password != r.PostFormValue("confirm") {
		s.renderEntry(w, "signup", http.StatusUnprocessableEntity, email, "As senhas não são iguais.")
		return
	}

	// Counted before hashing, because each hash costs real CPU.
	s.signupLimit.Record(clientIP)
	hash, err := HashPassword(password)
	if err != nil {
		s.cfg.Logger.Error("signup: hash password", "err", err)
		s.renderEntry(w, "signup", http.StatusInternalServerError, email, msgBadSignup)
		return
	}
	user, err := s.store.CreateUser(r.Context(), email, hash)
	if errors.Is(err, store.ErrEmailTaken) {
		s.renderEntry(w, "signup", http.StatusConflict, email, "Este e-mail já tem conta. Entre com a sua senha.")
		return
	}
	if err != nil {
		s.cfg.Logger.Error("signup: create user", "err", err)
		s.renderEntry(w, "signup", http.StatusInternalServerError, email, msgBadSignup)
		return
	}
	if err := s.startSession(w, r, user); err != nil {
		s.cfg.Logger.Error("signup: start session", "err", err)
		s.renderEntry(w, "signup", http.StatusInternalServerError, email, msgBadSignup)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("solarguard_session"); err == nil {
		s.sm.RevokeSession(cookie.Value)
	}
	s.setSessionCookie(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// sessionUserID returns the account of the request. The auth middleware
// guarantees a session on every route that reaches a handler using this.
func sessionUserID(r *http.Request) int64 {
	sess, ok := r.Context().Value(sessionContextKey).(*Session)
	if !ok || sess == nil {
		return 0
	}
	return sess.UserID
}

// userPlant returns a plant of the account. Another account's plant is reported
// as not found, so the response does not reveal that it exists.
func (s *Server) userPlant(r *http.Request, plantID int64) (domain.Plant, error) {
	owned, err := s.store.PlantOwnedBy(r.Context(), plantID, sessionUserID(r))
	if err != nil {
		return domain.Plant{}, err
	}
	if !owned {
		return domain.Plant{}, store.ErrNotFound
	}
	return s.store.GetPlant(r.Context(), plantID)
}

// userAlert returns an alert of one of the account's plants.
func (s *Server) userAlert(r *http.Request, alertID int64) (store.Alert, error) {
	alert, err := s.store.GetAlert(r.Context(), alertID)
	if err != nil {
		return store.Alert{}, err
	}
	if _, err := s.userPlant(r, alert.PlantID); err != nil {
		return store.Alert{}, err
	}
	return alert, nil
}

// userReport returns a report of one of the account's plants. A report without
// a plant covers every plant in the database, so no account can open it.
func (s *Server) userReport(r *http.Request, reportID int64) (store.Report, error) {
	rep, err := s.store.GetReport(r.Context(), reportID)
	if err != nil {
		return store.Report{}, err
	}
	if rep.PlantID == nil {
		return store.Report{}, store.ErrNotFound
	}
	if _, err := s.userPlant(r, *rep.PlantID); err != nil {
		return store.Report{}, err
	}
	return rep, nil
}

func (s *Server) notFoundOrError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.cfg.Logger.Error("check owner", "path", r.URL.Path, "err", err)
	http.Error(w, "Erro interno", http.StatusInternalServerError)
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, accountMessage(r.URL.Query()), "")
}

func accountMessage(q map[string][]string) string {
	values, ok := q["ok"]
	if !ok || len(values) == 0 {
		return ""
	}
	switch values[0] {
	case "conectado":
		return "Token salvo. A sincronização começou."
	case "removido":
		return "Token removido. A sincronização parou e os dados continuam na sua conta."
	}
	return ""
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, success, failure string) {
	ctx := r.Context()
	userID := sessionUserID(r)
	sources, err := s.store.ListSourcesForUser(ctx, userID)
	if err != nil {
		s.cfg.Logger.Error("account: list sources", "err", err)
		http.Error(w, "Erro ao carregar a conta", http.StatusInternalServerError)
		return
	}
	plants, err := s.store.ListPlantsForUser(ctx, userID)
	if err != nil {
		s.cfg.Logger.Error("account: list plants", "err", err)
		http.Error(w, "Erro ao carregar a conta", http.StatusInternalServerError)
		return
	}

	var stats store.QueueStats
	hasToken, collecting := false, false
	for _, src := range sources {
		if src.Kind == "growatt" && src.CredentialEnc != "" {
			hasToken = true
			collecting = s.cfg.Tokens != nil && s.cfg.Tokens.Collecting(src.ID)
			if stats, err = s.store.GetQueueStats(ctx, src.ID); err != nil {
				s.cfg.Logger.Error("account: queue stats", "err", err)
			}
		}
	}

	data := s.baseTemplateData(r, "Conta", "account")
	data["HasToken"] = hasToken
	data["Collecting"] = collecting
	data["PlantCount"] = len(plants)
	data["Stats"] = stats
	data["TokensAvailable"] = s.cfg.Tokens != nil
	data["SuccessMessage"] = success
	data["ErrorMessage"] = failure

	w.WriteHeader(status)
	if err := s.render(w, "account", data); err != nil {
		s.cfg.Logger.Error("render account", "err", err)
	}
}

func (s *Server) handleAccountTokenSave(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tokens == nil {
		s.renderAccount(w, r, http.StatusServiceUnavailable, "", "Este servidor não está configurado para receber tokens.")
		return
	}
	userID := sessionUserID(r)
	limitKey := strconv.FormatInt(userID, 10)
	if s.tokenLimit.Blocked(limitKey) {
		s.renderAccount(w, r, http.StatusTooManyRequests, "", msgTooMany)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" || len(token) > maxToken {
		s.renderAccount(w, r, http.StatusUnprocessableEntity, "", "Informe um token válido.")
		return
	}

	// A closed browser tab must not interrupt a token change halfway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), connectTimeout)
	defer cancel()
	_, err := s.cfg.Tokens.Connect(ctx, userID, token)
	if err == nil {
		s.tokenLimit.Clear(limitKey)
		http.Redirect(w, r, "/account?ok=conectado", http.StatusSeeOther)
		return
	}

	status, msg := http.StatusInternalServerError, "Não foi possível salvar o token. Tente novamente."
	switch {
	case errors.Is(err, connector.ErrRateLimited):
		status, msg = http.StatusTooManyRequests, "A Growatt só aceita esta consulta a cada 5 minutos. Aguarde e tente novamente."
	case errors.Is(err, ingest.ErrPlantsOwned):
		status, msg = http.StatusConflict, "As usinas deste token já estão vinculadas a outra conta."
	case errors.Is(err, ingest.ErrTokenRejected):
		s.tokenLimit.Record(limitKey)
		status, msg = http.StatusUnprocessableEntity, "Não foi possível validar o token na Growatt. Confira o token, a permissão de API dele e tente novamente."
	case errors.Is(err, secrets.ErrNoKey):
		status, msg = http.StatusServiceUnavailable, "Este servidor não está configurado para receber tokens."
	}
	level := slog.LevelError
	if status < http.StatusInternalServerError {
		level = slog.LevelInfo
	}
	s.cfg.Logger.Log(r.Context(), level, "connect token", "user_id", userID, "err", err)
	s.renderAccount(w, r, status, "", msg)
}

func (s *Server) handleAccountTokenRemove(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tokens == nil {
		s.renderAccount(w, r, http.StatusServiceUnavailable, "", "Este servidor não está configurado para receber tokens.")
		return
	}
	if err := s.cfg.Tokens.Disconnect(r.Context(), sessionUserID(r)); err != nil {
		s.cfg.Logger.Error("disconnect token", "err", err)
		s.renderAccount(w, r, http.StatusInternalServerError, "", "Não foi possível remover o token. Tente novamente.")
		return
	}
	http.Redirect(w, r, "/account?ok=removido", http.StatusSeeOther)
}
