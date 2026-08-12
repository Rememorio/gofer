package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testSecret = "01234567890123456789012345678901"

func TestStaticTokensAuthenticationIsolation(t *testing.T) {
	t.Parallel()
	tokens, err := NewStaticTokens([]Token{{Secret: testSecret, Principal: Principal{ID: "user", Permissions: []Permission{RunsRead, ThreadsRead}}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "bearer "+testSecret)
	principal, err := tokens.Authenticate(context.Background(), request)
	if err != nil || principal.ID != "user" || !principal.Has(RunsRead) || principal.Has(RunsCreate) {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	principal.Permissions[0] = Admin
	again, _ := tokens.Authenticate(context.Background(), request)
	if again.Permissions[0] == Admin {
		t.Fatal("permissions share storage")
	}
}

func TestStaticTokensRejectInvalidCredentialsAndConfig(t *testing.T) {
	t.Parallel()
	valid := Token{Secret: testSecret, Principal: Principal{ID: "u", Permissions: []Permission{ThreadsRead}}}
	tests := [][]Token{nil, {{Secret: "short", Principal: valid.Principal}}, {{Secret: testSecret, Principal: Principal{}}}, {valid, valid}, {{Secret: testSecret, Principal: valid.Principal}, {Secret: testSecret, Principal: Principal{ID: "v", Permissions: []Permission{ThreadsRead}}}}}
	for _, candidate := range tests {
		if _, err := NewStaticTokens(candidate); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewStaticTokens(%#v)=%v", candidate, err)
		}
	}
	tokens, _ := NewStaticTokens([]Token{valid})
	for _, header := range []string{"", "Basic abc", "Bearer wrong"} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		request.Header.Set("Authorization", header)
		if _, err := tokens.Authenticate(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Authenticate(%q)=%v", header, err)
		}
	}
	if _, err := (*StaticTokens)(nil).Authenticate(context.Background(), nil); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("nil=%v", err)
	}
}

func TestMiddlewareGatewayPolicyAndContext(t *testing.T) {
	t.Parallel()
	tokens, _ := NewStaticTokens([]Token{{Secret: testSecret, Principal: Principal{ID: "reader", Permissions: []Permission{ThreadsRead, RunsRead}}}})
	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		principal, ok := PrincipalFromContext(request.Context())
		if !ok || principal.ID != "reader" {
			t.Error("missing principal")
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	handler, err := (Middleware{Authenticator: tokens, Policy: GatewayPolicy, Next: next}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string) int {
		request := httptest.NewRequestWithContext(context.Background(), method, path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	if got := call(http.MethodGet, "/healthz", ""); got != http.StatusNoContent {
		t.Fatalf("health=%d", got)
	}
	if got := call(http.MethodGet, "/api/threads/x", ""); got != http.StatusUnauthorized {
		t.Fatalf("unauth=%d", got)
	}
	if got := call(http.MethodPost, "/api/threads/x/runs", testSecret); got != http.StatusForbidden {
		t.Fatalf("forbidden=%d", got)
	}
	if got := call(http.MethodGet, "/api/threads/x/runs/y", testSecret); got != http.StatusNoContent {
		t.Fatalf("allowed=%d", got)
	}
	if got := call(http.MethodGet, "/unknown", testSecret); got != http.StatusForbidden {
		t.Fatalf("unknown=%d", got)
	}
}

func TestGatewayPolicyResourceRoutes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		method, path string
		permission   Permission
	}{
		{http.MethodGet, "/api/scheduled-tasks", ScheduledRead},
		{http.MethodGet, "/api/scheduled-tasks/task", ScheduledRead},
		{http.MethodPost, "/api/scheduled-tasks", ScheduledWrite},
		{http.MethodPatch, "/api/scheduled-tasks/task", ScheduledWrite},
		{http.MethodPost, "/api/scheduled-tasks/task/trigger", ScheduledWrite},
		{http.MethodGet, "/api/scheduled-tasks-extra", Admin},
		{http.MethodPost, "/api/threads/search", ThreadsRead},
		{http.MethodGet, "/api/models", ResourcesRead},
		{http.MethodGet, "/api/skills/demo", ResourcesRead},
		{http.MethodGet, "/api/skills-extra", Admin},
		{http.MethodPost, "/api/skills/demo/enable", ResourcesWrite},
		{http.MethodPost, "/api/threads/x/uploads", ResourcesWrite},
		{http.MethodGet, "/api/threads/x/artifacts/mnt/user-data/outputs/x", ResourcesRead},
	} {
		request := httptest.NewRequestWithContext(context.Background(), test.method, test.path, nil)
		permission, public := GatewayPolicy(request)
		if public || permission != test.permission {
			t.Fatalf("GatewayPolicy(%s %s) = %q, %v", test.method, test.path, permission, public)
		}
	}
}

func TestMiddlewareAndContextValidation(t *testing.T) {
	t.Parallel()
	if _, err := (Middleware{}).Handler(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Handler=%v", err)
	}
	principal := Principal{ID: "u", Permissions: []Permission{Admin}}
	ctx := WithPrincipal(context.Background(), principal)
	got, ok := PrincipalFromContext(ctx)
	if !ok || !got.Has(ThreadsDelete) {
		t.Fatalf("principal=%#v", got)
	}
	got.Permissions[0] = ThreadsRead
	again, _ := PrincipalFromContext(ctx)
	if again.Permissions[0] != Admin {
		t.Fatal("context shared permissions")
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty context has principal")
	}
}
