package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/storage"
)

type ctxKeyUserID struct{}

// UserIDFromContext extracts the authenticated numeric Telegram user ID injected by AuthMiddleware.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKeyUserID{}).(int64)
	return v, ok && v > 0
}

// WithUserID attaches an authenticated Telegram user ID to ctx.
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, ctxKeyUserID{}, userID)
}

// TokenValidator validates a raw Bearer token and returns its numeric Telegram user ID.
type TokenValidator interface {
	ValidateAuthToken(ctx context.Context, rawToken string, now time.Time) (int64, error)
}

var _ TokenValidator = (*storage.Store)(nil)

// AuthMiddleware enforces Authorization: Bearer <token> on every HTTP request,
// resolving the caller's telegram_user_id into request context.
func AuthMiddleware(validator TokenValidator, nowFunc func() time.Time, next http.Handler) http.Handler {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			writeUnauthorized(w, "missing bearer token")
			return
		}
		rawToken := strings.TrimSpace(authz[len("bearer "):])
		if rawToken == "" {
			writeUnauthorized(w, "empty bearer token")
			return
		}

		userID, err := validator.ValidateAuthToken(r.Context(), rawToken, nowFunc())
		if err != nil {
			writeUnauthorized(w, "invalid, revoked, or expired bearer token")
			return
		}

		next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), userID)))
	})
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="mcpgram"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
	})
}
