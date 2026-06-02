package httpapi

import (
	"context"
	"net/http"
)

type Role string

const (
	RoleViewer    Role = "viewer"
	RoleResponder Role = "responder"
	RoleCommander Role = "commander"
	RoleAdmin     Role = "admin"
)

type roleKey struct{}

func withRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("X-Role")
		role := Role(raw)
		if !isKnownRole(role) {
			role = RoleViewer
		}
		ctx := context.WithValue(r.Context(), roleKey{}, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func roleFromContext(ctx context.Context) Role {
	v, ok := ctx.Value(roleKey{}).(Role)
	if !ok {
		return RoleViewer
	}
	return v
}

func isKnownRole(role Role) bool {
	switch role {
	case RoleViewer, RoleResponder, RoleCommander, RoleAdmin:
		return true
	default:
		return false
	}
}
