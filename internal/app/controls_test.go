package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
)

func TestThreadControlHTTPWorkflow(t *testing.T) {
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
	threadID := createThread(t, server.URL, "")
	goalPath := "/api/threads/" + string(threadID) + "/goal"
	controlPath := "/api/threads/" + string(threadID) + "/control"
	todosPath := "/api/threads/" + string(threadID) + "/todos"

	empty := resourceRequest[struct {
		Goal *control.Goal `json:"goal"`
	}](t, server.URL, http.MethodGet, goalPath, nil, "", http.StatusOK)
	if empty.Goal != nil {
		t.Fatalf("empty goal = %#v", empty.Goal)
	}
	set := resourceRequest[struct {
		Goal *control.Goal `json:"goal"`
	}](t, server.URL, http.MethodPut, goalPath, map[string]any{"objective": "Ship Gofer", "token_budget": 5000}, "", http.StatusOK)
	if set.Goal == nil || set.Goal.Objective != "Ship Gofer" || set.Goal.TokenBudget != 5000 {
		t.Fatalf("set goal = %#v", set)
	}
	state := resourceRequest[control.State](t, server.URL, http.MethodPut, todosPath, map[string]any{"todos": []map[string]string{{"step": "test", "status": "completed"}}}, "", http.StatusOK)
	if state.Version != 2 || len(state.Todos) != 1 || state.Todos[0].ID != "todo-1" {
		t.Fatalf("todos = %#v", state)
	}
	state = resourceRequest[control.State](t, server.URL, http.MethodGet, controlPath, nil, "", http.StatusOK)
	if state.Goal == nil || len(state.Todos) != 1 {
		t.Fatalf("control = %#v", state)
	}
	resourceRequest[map[string]any](t, server.URL, http.MethodDelete, goalPath, nil, "", http.StatusOK)
	state = resourceRequest[control.State](t, server.URL, http.MethodGet, controlPath, nil, "", http.StatusOK)
	if state.Goal != nil || len(state.Todos) != 0 {
		t.Fatalf("cleared control = %#v", state)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPut, goalPath, map[string]any{"objective": ""}, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPut, goalPath, map[string]any{"objective": "x", "unknown": true}, "", http.StatusUnprocessableEntity)
	missing, _ := domain.NewThreadID()
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/threads/"+string(missing)+"/goal", nil, "", http.StatusNotFound)
}

func TestThreadControlRejectsMutationDuringRun(t *testing.T) {
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
	threadID := createThread(t, server.URL, "")
	run, err := domain.NewRun(threadID, time.Now())
	if err != nil || service.store.CreateRun(context.Background(), run) != nil {
		t.Fatalf("create pending run: %v", err)
	}
	path := "/api/threads/" + string(threadID) + "/goal"
	resourceRequest[map[string]string](t, server.URL, http.MethodPut, path, map[string]string{"objective": "blocked"}, "", http.StatusConflict)
}

func TestSQLiteControlStateWiresIntoServiceRestart(t *testing.T) {
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
	threadID := createThread(t, server.URL, "")
	path := "/api/threads/" + string(threadID) + "/goal"
	resourceRequest[map[string]any](t, server.URL, http.MethodPut, path, map[string]string{"objective": "survive restart"}, "", http.StatusOK)
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
	goal := resourceRequest[struct {
		Goal *control.Goal `json:"goal"`
	}](t, server.URL, http.MethodGet, path, nil, "", http.StatusOK)
	if goal.Goal == nil || goal.Goal.Objective != "survive restart" {
		t.Fatalf("restarted goal = %#v", goal)
	}
}
