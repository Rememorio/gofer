package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/config"
)

func TestMemoryHTTPWorkflow(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	assertEmptyMemoryAndConfig(t, server.URL)
	id := createAndPatchMemory(t, server.URL)
	if id == "" {
		t.Fatal("empty memory id")
	}
	importAndInspectMemory(t, server.URL)
	deleteAndClearMemory(t, server.URL)
}

func assertEmptyMemoryAndConfig(t *testing.T, baseURL string) {
	t.Helper()
	empty := resourceRequest[memoryResponse](t, baseURL, http.MethodGet, "/api/memory", nil, "", http.StatusOK)
	if empty.Version != "1.0" || len(empty.Facts) != 0 {
		t.Fatalf("empty memory = %#v", empty)
	}
	cfg := resourceRequest[memoryConfigResponse](t, baseURL, http.MethodGet, "/api/memory/config", nil, "", http.StatusOK)
	if !cfg.Enabled || cfg.ManagerClass != "gofer" || cfg.BackendConfig["driver"] != "memory" {
		t.Fatalf("memory config = %#v", cfg)
	}
}

func createAndPatchMemory(t *testing.T, baseURL string) string {
	t.Helper()
	created := resourceRequest[memoryResponse](t, baseURL, http.MethodPost, "/api/memory/facts", map[string]any{"content": "Use Go for services", "category": "preference", "topics": []string{"code"}, "confidence": 0.9, "source": "manual", "ttl_seconds": 60}, "", http.StatusOK)
	if len(created.Facts) != 1 || created.Facts[0].ID == "" || created.Facts[0].Content != "Use Go for services" || created.Facts[0].ExpiresAt == "" {
		t.Fatalf("created memory = %#v", created)
	}
	id := created.Facts[0].ID
	filtered := resourceRequest[memoryResponse](t, baseURL, http.MethodGet, "/api/memory?q=Go&tags=code&limit=1", nil, "", http.StatusOK)
	if len(filtered.Facts) != 1 || filtered.Facts[0].ID != id {
		t.Fatalf("filtered memory = %#v", filtered)
	}
	expires := time.Now().UTC().Add(time.Hour)
	patched := resourceRequest[memoryResponse](t, baseURL, http.MethodPatch, "/api/memory/facts/"+id, map[string]any{"content": "Prefer Go", "category": "profile", "topics": []string{"golang"}, "confidence": 0.95, "source": "edited", "expires_at": expires}, "", http.StatusOK)
	if len(patched.Facts) != 1 || patched.Facts[0].Content != "Prefer Go" || patched.Facts[0].Confidence != 0.95 || patched.Facts[0].Category != "profile" || patched.Facts[0].Source != "edited" {
		t.Fatalf("patched memory = %#v", patched)
	}
	exported := resourceRequest[memoryResponse](t, baseURL, http.MethodGet, "/api/memory/export", nil, "", http.StatusOK)
	if len(exported.Facts) != 1 || exported.LastUpdated == "" {
		t.Fatalf("exported memory = %#v", exported)
	}
	return id
}

func importAndInspectMemory(t *testing.T, baseURL string) {
	t.Helper()
	importedFact := memoryFact{ID: "imported", Content: "Imported fact", Category: "context", Confidence: 0.8, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Source: "backup"}
	imported := resourceRequest[memoryResponse](t, baseURL, http.MethodPost, "/api/memory/import", memoryResponse{Version: "1.0", Facts: []memoryFact{importedFact}}, "", http.StatusOK)
	if len(imported.Facts) != 1 || imported.Facts[0].ID != "imported" {
		t.Fatalf("imported memory = %#v", imported)
	}
	status := resourceRequest[struct {
		Config memoryConfigResponse `json:"config"`
		Data   memoryResponse       `json:"data"`
	}](t, baseURL, http.MethodGet, "/api/memory/status", nil, "", http.StatusOK)
	if !status.Config.Enabled || len(status.Data.Facts) != 1 {
		t.Fatalf("memory status = %#v", status)
	}
	reloaded := resourceRequest[memoryResponse](t, baseURL, http.MethodPost, "/api/memory/reload", nil, "", http.StatusOK)
	if len(reloaded.Facts) != 1 {
		t.Fatalf("reloaded memory = %#v", reloaded)
	}
}

func deleteAndClearMemory(t *testing.T, baseURL string) {
	t.Helper()
	resourceRequest[memoryResponse](t, baseURL, http.MethodDelete, "/api/memory/facts/imported", nil, "", http.StatusOK)
	resourceRequest[map[string]string](t, baseURL, http.MethodDelete, "/api/memory/facts/missing", nil, "", http.StatusNotFound)
	resourceRequest[memoryResponse](t, baseURL, http.MethodPost, "/api/memory/facts", map[string]string{"content": "one"}, "", http.StatusOK)
	cleared := resourceRequest[memoryResponse](t, baseURL, http.MethodDelete, "/api/memory", nil, "", http.StatusOK)
	if len(cleared.Facts) != 0 {
		t.Fatalf("cleared memory = %#v", cleared)
	}
}

func TestMemoryHTTPValidationAndDisabledMode(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]string{"content": ""}, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]any{"content": "x", "ttl_seconds": -1}, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/memory?limit=101", nil, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPatch, "/api/memory/facts/missing", map[string]string{"content": "x"}, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/import", memoryResponse{Facts: []memoryFact{{ID: "bad", Content: "x", CreatedAt: "not-a-time"}}}, "", http.StatusUnprocessableEntity)
	server.Close()
	_ = service.Close()

	cfg.Memory.Enabled = false
	disabled, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disabled.Close() }()
	server = httptest.NewServer(disabled.Handler())
	defer server.Close()
	memoryConfig := resourceRequest[memoryConfigResponse](t, server.URL, http.MethodGet, "/api/memory/config", nil, "", http.StatusOK)
	if memoryConfig.Enabled {
		t.Fatalf("disabled config = %#v", memoryConfig)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/memory", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/reload", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodDelete, "/api/memory", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]string{"content": "x"}, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/memory/status", nil, "", http.StatusNotFound)
}

func TestSQLiteMemoryWiresIntoServiceRestart(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	directory := t.TempDir()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Storage = config.StorageConfig{Driver: "sqlite", DSN: filepath.Join(directory, "gofer.db")}
	cfg.Workspace.Root = filepath.Join(directory, "workspaces")
	first, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(first.Handler())
	resourceRequest[memoryResponse](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]string{"content": "survive restart"}, "", http.StatusOK)
	server.Close()
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	server = httptest.NewServer(second.Handler())
	defer server.Close()
	data := resourceRequest[memoryResponse](t, server.URL, http.MethodGet, "/api/memory", nil, "", http.StatusOK)
	if len(data.Facts) != 1 || data.Facts[0].Content != "survive restart" {
		t.Fatalf("restarted memory = %#v", data)
	}
}

func TestMemoryHTTPAuthorizationAndOwnerIsolation(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	aliceToken, bobToken, readerToken := "alice-memory-token-00000001", "bob-memory-token-0000000002", "reader-memory-token-0000003"
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Auth = config.AuthConfig{Enabled: true, Tokens: []config.AuthTokenConfig{
		{Secret: aliceToken, PrincipalID: "alice", Permissions: []string{string(auth.MemoryRead), string(auth.MemoryWrite)}},
		{Secret: bobToken, PrincipalID: "bob", Permissions: []string{string(auth.MemoryRead), string(auth.MemoryWrite)}},
		{Secret: readerToken, PrincipalID: "reader", Permissions: []string{string(auth.MemoryRead)}},
	}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	authorizedMemoryRequest[memoryResponse](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]string{"content": "alice secret"}, aliceToken, http.StatusOK)
	bob := authorizedMemoryRequest[memoryResponse](t, server.URL, http.MethodGet, "/api/memory", nil, bobToken, http.StatusOK)
	if len(bob.Facts) != 0 {
		t.Fatalf("bob memory = %#v", bob)
	}
	authorizedMemoryRequest[map[string]string](t, server.URL, http.MethodPost, "/api/memory/facts", map[string]string{"content": "denied"}, readerToken, http.StatusForbidden)
	authorizedMemoryRequest[map[string]string](t, server.URL, http.MethodGet, "/api/memory", nil, "", http.StatusUnauthorized)
}

func authorizedMemoryRequest[T any](t *testing.T, baseURL, method, path string, body any, token string, wantStatus int) T {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != wantStatus {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status = %d, want %d: %s", method, path, response.StatusCode, wantStatus, data)
	}
	var result T
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
