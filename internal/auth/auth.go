package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

var (
	// ErrUnauthenticated identifies a missing or invalid credential.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden identifies an authenticated principal lacking permission.
	ErrForbidden = errors.New("forbidden")
	// ErrInvalidConfig identifies malformed authentication configuration.
	ErrInvalidConfig = errors.New("invalid authentication configuration")
)

// Permission is one stable authorization capability.
type Permission string

const (
	// ThreadsRead permits reading thread resources.
	ThreadsRead Permission = "threads:read"
	// ThreadsWrite permits creating and changing thread resources.
	ThreadsWrite Permission = "threads:write"
	// ThreadsDelete permits deleting thread resources.
	ThreadsDelete Permission = "threads:delete"
	// RunsCreate permits launching runs.
	RunsCreate Permission = "runs:create"
	// RunsRead permits reading run state and events.
	RunsRead Permission = "runs:read"
	// RunsCancel permits cancelling runs.
	RunsCancel Permission = "runs:cancel"
	// Admin permits all operations.
	Admin Permission = "admin"
	// ScheduledRead permits reading scheduled tasks.
	ScheduledRead Permission = "scheduled:read"
	// ScheduledWrite permits creating and changing scheduled tasks.
	ScheduledWrite Permission = "scheduled:write"
	// ResourcesRead permits model, feature, skill, upload, and artifact discovery.
	ResourcesRead Permission = "resources:read"
	// ResourcesWrite permits skill state changes and file uploads or deletion.
	ResourcesWrite Permission = "resources:write"
)

// Principal is an authenticated identity with bounded permissions.
type Principal struct {
	ID          string       `json:"id"`
	Permissions []Permission `json:"permissions"`
}

// Has reports whether principal has permission or the admin capability.
func (principal Principal) Has(permission Permission) bool {
	for _, candidate := range principal.Permissions {
		if candidate == permission || candidate == Admin {
			return true
		}
	}
	return false
}

// Token configures one opaque bearer secret and its resulting principal.
type Token struct {
	Secret    string
	Principal Principal
}

// StaticTokens stores only SHA-256 token digests after construction.
type StaticTokens struct {
	principals map[[sha256.Size]byte]Principal
}

// NewStaticTokens validates unique credentials and constructs an authenticator.
func NewStaticTokens(tokens []Token) (*StaticTokens, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: at least one token is required", ErrInvalidConfig)
	}
	store := &StaticTokens{principals: make(map[[sha256.Size]byte]Principal, len(tokens))}
	ids := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		token.Secret = strings.TrimSpace(token.Secret)
		token.Principal.ID = strings.TrimSpace(token.Principal.ID)
		if len(token.Secret) < 24 || token.Principal.ID == "" || len(token.Principal.Permissions) == 0 {
			return nil, fmt.Errorf("%w: token secret, principal, and permissions are required", ErrInvalidConfig)
		}
		if _, exists := ids[token.Principal.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate principal", ErrInvalidConfig)
		}
		ids[token.Principal.ID] = struct{}{}
		digest := sha256.Sum256([]byte(token.Secret))
		if _, exists := store.principals[digest]; exists {
			return nil, fmt.Errorf("%w: duplicate token", ErrInvalidConfig)
		}
		principal := token.Principal
		principal.Permissions = append([]Permission(nil), principal.Permissions...)
		sort.Slice(principal.Permissions, func(i, j int) bool { return principal.Permissions[i] < principal.Permissions[j] })
		store.principals[digest] = principal
	}
	return store, nil
}

// Authenticate validates an RFC 6750 Bearer credential.
func (store *StaticTokens) Authenticate(_ context.Context, request *http.Request) (Principal, error) {
	if store == nil || request == nil {
		return Principal{}, ErrUnauthenticated
	}
	scheme, secret, ok := strings.Cut(strings.TrimSpace(request.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(secret) == "" {
		return Principal{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	principal, exists := store.principals[digest]
	if !exists {
		return Principal{}, ErrUnauthenticated
	}
	principal.Permissions = append([]Permission(nil), principal.Permissions...)
	return principal, nil
}

// Authenticator resolves an HTTP request identity.
type Authenticator interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
}

// Policy resolves the permission required by one request. The bool reports public access.
type Policy func(*http.Request) (Permission, bool)

// Middleware applies authentication and permission checks before serving next.
type Middleware struct {
	Authenticator Authenticator
	Policy        Policy
	Next          http.Handler
}

// Handler validates dependencies and returns a fail-closed HTTP handler.
func (middleware Middleware) Handler() (http.Handler, error) {
	if middleware.Authenticator == nil || middleware.Policy == nil || middleware.Next == nil {
		return nil, fmt.Errorf("%w: authenticator, policy, and next handler are required", ErrInvalidConfig)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		permission, public := middleware.Policy(request)
		if public {
			middleware.Next.ServeHTTP(writer, request)
			return
		}
		principal, err := middleware.Authenticator.Authenticate(request.Context(), request)
		if err != nil {
			writeError(writer, http.StatusUnauthorized)
			return
		}
		if !principal.Has(permission) {
			writeError(writer, http.StatusForbidden)
			return
		}
		middleware.Next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), principal)))
	}), nil
}

// GatewayPolicy maps Gofer gateway routes to stable capabilities.
func GatewayPolicy(request *http.Request) (Permission, bool) {
	path := request.URL.Path
	if path == "/healthz" {
		return "", true
	}
	if permission, matched := resourcePermission(request); matched {
		return permission, false
	}
	if strings.Contains(path, "/runs/") && strings.HasSuffix(path, "/cancel") {
		return RunsCancel, false
	}
	if strings.Contains(path, "/runs") {
		if request.Method == http.MethodPost {
			return RunsCreate, false
		}
		return RunsRead, false
	}
	if strings.HasPrefix(path, "/api/threads") {
		if path == "/api/threads/search" && request.Method == http.MethodPost {
			return ThreadsRead, false
		}
		if request.Method == http.MethodGet {
			return ThreadsRead, false
		}
		if request.Method == http.MethodDelete {
			return ThreadsDelete, false
		}
		return ThreadsWrite, false
	}
	return Admin, false
}

func resourcePermission(request *http.Request) (Permission, bool) {
	path := request.URL.Path
	resourcePath := path == "/api/models" || strings.HasPrefix(path, "/api/models/") || path == "/api/features" ||
		path == "/api/skills" || strings.HasPrefix(path, "/api/skills/") ||
		strings.HasPrefix(path, "/api/threads/") && (strings.Contains(path, "/uploads") || strings.Contains(path, "/artifacts"))
	if resourcePath {
		if request.Method == http.MethodGet {
			return ResourcesRead, true
		}
		return ResourcesWrite, true
	}
	if path == "/api/scheduled-tasks" || strings.HasPrefix(path, "/api/scheduled-tasks/") {
		if request.Method == http.MethodGet {
			return ScheduledRead, true
		}
		return ScheduledWrite, true
	}
	return "", false
}

type principalKey struct{}

// WithPrincipal stores an isolated principal in context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	principal.Permissions = append([]Permission(nil), principal.Permissions...)
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext returns the authenticated principal if present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return Principal{}, false
	}
	principal.Permissions = append([]Permission(nil), principal.Permissions...)
	return principal, true
}

func writeError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, "{\"error\":%q}\n", http.StatusText(status))
}
