package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coachpo/cloudex/apps/api/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "cloudex_session"
	sessionTTL = 12 * time.Hour
)

type userKey struct{}
type sessionKey struct{}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func validCredentials(w http.ResponseWriter, r *http.Request, username, password string, passwordOptional bool) bool {
	if len(username) < 1 || len(username) > 64 || username != strings.TrimSpace(username) {
		problem(w, r, 400, "INVALID_USERNAME", "Use a username of 1–64 bytes without leading or trailing whitespace.")
		return false
	}
	if (!passwordOptional || password != "") && (len(password) < 12 || len(password) > 72) {
		problem(w, r, 400, "INVALID_PASSWORD_LENGTH", "Use a password of 12–72 bytes.")
		return false
	}
	return true
}

func (s *Server) cookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true,
		Secure: strings.HasPrefix(s.Origin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request, token, user string) {
	s.cookie(w, token, int(sessionTTL.Seconds()))
	reply(w, r, 200, map[string]string{"username": user})
}
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	required, err := s.Store.SetupRequired(r.Context())
	send(w, r, map[string]bool{"required": required}, err)
}
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !parse(w, r, &in, false) || !validCredentials(w, r, in.Username, in.Password, false) {
		return
	}
	token := rand.Text() + rand.Text()
	err := s.Store.CreateAdmin(r.Context(), in.Username, in.Password, digest(token), time.Now().Add(sessionTTL))
	switch {
	case errors.Is(err, store.ErrSetupCompleted):
		problem(w, r, 409, "SETUP_COMPLETED", "The administrator account has already been created.")
	case err != nil:
		failure(w, r, err)
	default:
		s.signedIn(w, r, token, in.Username)
	}
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !parse(w, r, &in, false) {
		return
	}
	user, hash, e := s.Store.PasswordHash(r.Context())
	if errors.Is(e, store.ErrMissing) {
		problem(w, r, 409, "SETUP_REQUIRED", "Create the administrator account before signing in.")
		return
	}
	if e != nil {
		failure(w, r, e)
		return
	}
	if len(in.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || user != in.Username {
		problem(w, r, 401, "INVALID_LOGIN", "Incorrect username or password.")
		return
	}
	token := rand.Text() + rand.Text()
	e = s.Store.NewSession(r.Context(), digest(token), user, hash, time.Now().Add(sessionTTL))
	if errors.Is(e, store.ErrMissing) {
		problem(w, r, 401, "INVALID_LOGIN", "Incorrect username or password.")
		return
	}
	if e != nil {
		failure(w, r, e)
		return
	}
	s.signedIn(w, r, token, user)
}
func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		Username        string `json:"username"`
		NewPassword     string `json:"new_password"`
	}
	if !parse(w, r, &in, false) || !validCredentials(w, r, in.Username, in.NewPassword, true) {
		return
	}
	token := rand.Text() + rand.Text()
	err := s.Store.UpdateAdmin(r.Context(), r.Context().Value(sessionKey{}).(string),
		in.CurrentPassword, in.Username, in.NewPassword, digest(token), time.Now().Add(sessionTTL))
	switch {
	case errors.Is(err, store.ErrMissing):
		problem(w, r, 401, "UNAUTHORIZED", "Sign in to continue.")
	case errors.Is(err, store.ErrInvalidPassword):
		problem(w, r, 403, "INVALID_PASSWORD", "The current password is incorrect.")
	case err != nil:
		failure(w, r, err)
	default:
		s.signedIn(w, r, token, in.Username)
	}
}
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, e := r.Cookie(cookieName)
		if e != nil || len(ck.Value) > 100 {
			problem(w, r, 401, "UNAUTHORIZED", "Sign in to continue.")
			return
		}
		hashed := digest(ck.Value)
		user, e := s.Store.Session(r.Context(), hashed)
		if errors.Is(e, store.ErrMissing) {
			problem(w, r, 401, "UNAUTHORIZED", "Sign in to continue.")
			return
		}
		if e != nil {
			failure(w, r, e)
			return
		}
		ctx := context.WithValue(context.WithValue(r.Context(), userKey{}, user), sessionKey{}, hashed)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	reply(w, r, 200, map[string]any{"username": r.Context().Value(userKey{})})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if e := s.Store.EndSession(r.Context(), r.Context().Value(sessionKey{}).(string)); e != nil {
		failure(w, r, e)
		return
	}
	s.cookie(w, "", -1)
	reply(w, r, 200, map[string]bool{"signed_out": true})
}
