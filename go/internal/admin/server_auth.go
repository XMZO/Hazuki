package admin

import (
	"net/http"
	"net/url"
	"strings"

	"hazuki-go/internal/i18n"
	"hazuki-go/internal/storage"
)

func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	if st == nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	if st.HasUsers {
		if st.User != nil {
			http.Redirect(w, r, "/", http.StatusFound)
		} else {
			http.Redirect(w, r, "/login", http.StatusFound)
		}
		return
	}

	title := s.t(r, "page.setup.title")
	if !s.config.IsEncryptionEnabled() {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.masterKeyLoginDisabled"),
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
		})
		return
	case http.MethodPost:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := extractClientIP(r)
	if !s.limiter.check(clientIP) {
		w.Header().Set("Retry-After", "60")
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.tooManyAttempts"),
		})
		return
	}

	if err := r.ParseForm(); err != nil {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.badRequest"),
		})
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	_, err := storage.CreateUser(s.db, username, password)
	if err != nil {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        err.Error(),
		})
		return
	}

	user, ok, err := storage.VerifyUserPassword(s.db, username, password)
	if err != nil || !ok {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.failedCreateUser"),
		})
		return
	}
	token, err := storage.CreateSession(s.db, user.ID, s.sessionTTL)
	if err != nil {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "setup",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.failedCreateSession"),
		})
		return
	}

	setSessionCookie(w, token, s.sessionTTL, isSecureRequest(r))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	if st == nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	if !st.HasUsers {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if st.User != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	title := s.t(r, "page.login.title")
	if !s.config.IsEncryptionEnabled() {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.masterKeyLoginDisabled"),
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
		})
		return
	case http.MethodPost:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := extractClientIP(r)
	if !s.limiter.check(clientIP) {
		w.Header().Set("Retry-After", "60")
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.tooManyAttempts"),
		})
		return
	}

	if err := r.ParseForm(); err != nil {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.badRequest"),
		})
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	user, ok, err := storage.VerifyUserPassword(s.db, username, password)
	if err != nil {
		s.limiter.recordFailure(clientIP)
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.badRequest"),
		})
		return
	}
	if !ok {
		s.limiter.recordFailure(clientIP)
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.loginFailed"),
		})
		return
	}
	s.limiter.clearIP(clientIP)
	token, err := storage.CreateSession(s.db, user.ID, s.sessionTTL)
	if err != nil {
		s.render(w, r, layoutData{
			Title:        title,
			BodyTemplate: "login",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Error:        s.t(r, "error.failedCreateSession"),
		})
		return
	}
	setSessionCookie(w, token, s.sessionTTL, isSecureRequest(r))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *server) setLang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	next := ""
	if q := strings.TrimSpace(r.URL.Query().Get("next")); q != "" {
		if u, err := url.Parse(q); err == nil && !u.IsAbs() && strings.HasPrefix(u.Path, "/") {
			next = u.String()
		}
	}
	if next == "" {
		if ref := strings.TrimSpace(r.Referer()); ref != "" {
			if u, err := url.Parse(ref); err == nil && strings.EqualFold(u.Host, r.Host) && strings.HasPrefix(u.Path, "/") {
				next = u.RequestURI()
			}
		}
	}
	if next == "" {
		next = "/"
	}

	targetLang := i18n.NormalizeLang(r.URL.Query().Get("to"))
	if targetLang == "" {
		targetLang = i18n.LangZH
	}

	http.SetCookie(w, &http.Cookie{
		Name:     langCookieName,
		Value:    targetLang,
		Path:     "/",
		MaxAge:   31536000,
		Secure:   isSecureRequest(r),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, next, http.StatusFound)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	if st == nil || st.User == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		_ = storage.DeleteSession(s.db, cookie.Value)
	}
	clearSessionCookie(w, isSecureRequest(r))
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *server) account(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	title := s.t(r, "page.account.title")
	notice := ""
	if r.URL.Query().Get("ok") != "" {
		notice = s.t(r, "common.saved")
	}
	s.render(w, r, accountData{
		layoutData: layoutData{
			Title:        title,
			BodyTemplate: "account",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Notice:       notice,
		},
	})
}

func (s *server) accountPassword(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	title := s.t(r, "page.account.title")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, r, accountData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "account",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Error:        s.t(r, "error.badRequest"),
			},
		})
		return
	}

	currentPassword := r.FormValue("currentPassword")
	if _, ok, err := storage.VerifyUserPassword(s.db, st.User.Username, currentPassword); err != nil || !ok {
		errMsg := s.t(r, "error.currentPasswordWrong")
		if err != nil {
			errMsg = err.Error()
		}
		s.render(w, r, accountData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "account",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Error:        errMsg,
			},
		})
		return
	}

	newPassword := r.FormValue("newPassword")
	if err := storage.UpdateUserPassword(s.db, st.User.ID, newPassword); err != nil {
		s.render(w, r, accountData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "account",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Error:        err.Error(),
			},
		})
		return
	}

	// Invalidate all existing sessions for this user, then issue a fresh
	// one so the current browser stays logged in.  This ensures that any
	// stolen session token is revoked on password change.
	_ = storage.DeleteSessionsByUser(s.db, st.User.ID)
	token, err := storage.CreateSession(s.db, st.User.ID, s.sessionTTL)
	if err == nil {
		setSessionCookie(w, token, s.sessionTTL, isSecureRequest(r))
	}

	http.Redirect(w, r, "/account?ok=1", http.StatusFound)
}
