package httpapi

import (
	"context"
	"net/http"
	"strings"
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

func roleAllowed(role Role, method string, path string) bool {
	if role == RoleAdmin {
		return true
	}
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return true
	}

	if strings.HasPrefix(path, "/incidents") {
		return role == RoleResponder || role == RoleCommander
	}
	if path == "/reports/snapshots/materialize" {
		return role == RoleCommander
	}
	if strings.HasPrefix(path, "/integrations/") || strings.HasPrefix(path, "/ticket-templates/") {
		return role == RoleAdmin
	}
	return false
}

func isPublicPath(path string) bool {
	return path == "/health/live" || path == "/health/ready"
}
