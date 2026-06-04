package httpapi

import (
	"context"
	"net/http"
	"strings"
)

// Role is a named access tier for the API. Roles are passed by the caller via
// the X-Role request header and are trusted only after bearer-token
// authentication has already been verified by withAuthorization.
type Role string

const (
	RoleViewer    Role = "viewer"    // read-only access to all GET endpoints
	RoleResponder Role = "responder" // viewer + incident lifecycle mutations
	RoleCommander Role = "commander" // responder + snapshot materialisation
	RoleAdmin     Role = "admin"     // full access including integrations
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

// roleAllowed reports whether role may execute method on path. All GET/HEAD/OPTIONS
// requests are permitted regardless of role; write access is gated by the
// permission matrix defined here.
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
