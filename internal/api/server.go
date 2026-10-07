package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"deposit-escrow/internal/app"
	"deposit-escrow/internal/auth"

	"github.com/redis/go-redis/v9"
)

type Server struct {
	svc  *app.Service
	q    *app.Queries
	auth *auth.Manager
	rdb  *redis.Client
}

func NewServer(svc *app.Service, q *app.Queries, mgr *auth.Manager, rdb *redis.Client) *Server {
	return &Server{svc: svc, q: q, auth: mgr, rdb: rdb}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "NOT_FOUND", "message": err.Error()})
	case errors.Is(err, app.ErrValidation):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "VALIDATION", "message": err.Error()})
	case errors.Is(err, app.ErrUnresolved):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "UNRESOLVED_DISPUTE", "message": err.Error()})
	case errors.Is(err, app.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "CONFLICT", "message": err.Error()})
	case errors.Is(err, app.ErrBadState):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "BAD_STATE", "message": err.Error()})
	case errors.Is(err, app.ErrAmountExceeds):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "AMOUNT_EXCEEDS_DEPOSIT", "message": err.Error()})
	case errors.Is(err, app.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "FORBIDDEN", "message": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "INTERNAL", "message": err.Error()})
	}
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// withAuth 解析 Bearer token 并把 actor 注入 ctx。
func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, app.Actor)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "UNAUTHORIZED", "message": "missing bearer token"})
			return
		}
		sess, err := s.auth.Resolve(r.Context(), strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "UNAUTHORIZED", "message": "invalid or expired token"})
			return
		}
		next(w, r, sess.Actor)
	}
}

// idempotent 用 Redis SETNX 做客户端幂等(写接口可传 Idempotency-Key)。
func (s *Server) idempotent(r *http.Request, key string) (bool, func(bool)) {
	if key == "" {
		return false, func(bool) {}
	}
	lock := "deposit:idem:" + key
	ok, err := s.rdb.SetNX(r.Context(), lock, time.Now().UTC().Format(time.RFC3339), 10*time.Minute).Result()
	if err != nil || !ok {
		return false, func(bool) {} // Redis 异常时退化为不拦截
	}
	return true, func(commit bool) {
		if !commit {
			_ = s.rdb.Del(r.Context(), lock).Err()
		}
	}
}
