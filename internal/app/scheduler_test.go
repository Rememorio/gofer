package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/scheduler"
)

func TestScheduledTaskHTTPDispatchLifecycle(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if !bytes.Contains(body, []byte("scheduled prompt")) {
			t.Errorf("model request = %s", body)
		}
		calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer,
			`{"id":"scheduled","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`,
			`{"id":"scheduled","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	defer modelServer.Close()

	cfg := testConfig(t, modelServer.URL+"/v1")
	secret := strings.Repeat("t", 24)
	cfg.Auth = config.AuthConfig{Enabled: true, Tokens: []config.AuthTokenConfig{{
		Secret: secret, PrincipalID: "scheduler-user",
		Permissions: []string{string(auth.ScheduledRead), string(auth.ScheduledWrite), string(auth.RunsRead)},
	}}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	authorization := "Bearer " + secret

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	created := exerciseScheduledTaskDefinition(t, server.URL, authorization, future)
	dispatched := dispatchScheduledTask(t, server.URL, authorization, created, &calls)
	filtered := schedulerRequest[[]scheduler.Task](t, server.URL, http.MethodGet, "/api/scheduled-tasks?thread_id="+dispatched.ThreadID, nil, authorization, http.StatusOK)
	if len(filtered) != 1 || filtered[0].ID != created.ID {
		t.Fatalf("filtered = %#v", filtered)
	}
	schedulerRequest[struct{}](t, server.URL, http.MethodDelete, "/api/scheduled-tasks/"+created.ID, nil, authorization, http.StatusNoContent)
	schedulerRequest[map[string]string](t, server.URL, http.MethodGet, "/api/scheduled-tasks/"+created.ID, nil, authorization, http.StatusNotFound)
}

func exerciseScheduledTaskDefinition(t *testing.T, baseURL, authorization string, future time.Time) scheduler.Task {
	t.Helper()
	created := schedulerRequest[scheduler.Task](t, baseURL, http.MethodPost, "/api/scheduled-tasks", map[string]any{
		"title": "first title", "prompt": "scheduled prompt", "schedule_type": "once",
		"schedule": future.Format(time.RFC3339), "timezone": "UTC",
	}, authorization, http.StatusCreated)
	if created.UserID != "scheduler-user" || created.Status != scheduler.Enabled || !created.NextRunAt.Equal(future) {
		t.Fatalf("created = %#v", created)
	}

	listed := schedulerRequest[[]scheduler.Task](t, baseURL, http.MethodGet, "/api/scheduled-tasks", nil, authorization, http.StatusOK)
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %#v", listed)
	}
	got := schedulerRequest[scheduler.Task](t, baseURL, http.MethodGet, "/api/scheduled-tasks/"+created.ID, nil, authorization, http.StatusOK)
	if got.ID != created.ID {
		t.Fatalf("get = %#v", got)
	}
	updated := schedulerRequest[scheduler.Task](t, baseURL, http.MethodPatch, "/api/scheduled-tasks/"+created.ID, map[string]string{"title": "updated title"}, authorization, http.StatusOK)
	if updated.Title != "updated title" || !updated.NextRunAt.Equal(future) {
		t.Fatalf("updated = %#v", updated)
	}
	paused := schedulerRequest[scheduler.Task](t, baseURL, http.MethodPost, "/api/scheduled-tasks/"+created.ID+"/pause", nil, authorization, http.StatusOK)
	if paused.Status != scheduler.Paused {
		t.Fatalf("paused = %#v", paused)
	}
	resumed := schedulerRequest[scheduler.Task](t, baseURL, http.MethodPost, "/api/scheduled-tasks/"+created.ID+"/resume", nil, authorization, http.StatusOK)
	if resumed.Status != scheduler.Enabled {
		t.Fatalf("resumed = %#v", resumed)
	}
	return created
}

func dispatchScheduledTask(t *testing.T, baseURL, authorization string, created scheduler.Task, calls *atomic.Int32) scheduler.Task {
	t.Helper()
	dispatched := schedulerRequest[scheduler.Task](t, baseURL, http.MethodPost, "/api/scheduled-tasks/"+created.ID+"/trigger", nil, authorization, http.StatusOK)
	if dispatched.Status != scheduler.Completed || dispatched.RunCount != 1 || dispatched.LastRunID == "" || dispatched.ThreadID == "" {
		t.Fatalf("dispatched = %#v", dispatched)
	}
	threadID, err := domain.ParseThreadID(dispatched.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := domain.ParseRunID(dispatched.LastRunID)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, baseURL, threadID, runID, domain.RunSucceeded, authorization)
	if calls.Load() != 1 {
		t.Fatalf("model calls = %d", calls.Load())
	}
	return dispatched
}

func TestScheduledTaskHTTPErrorsAndOwnership(t *testing.T) {
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

	badBodies := []string{
		`{`,
		`{"title":"x","unknown":true}`,
		`{"title":"x"}{"title":"y"}`,
		`{"title":"x","prompt":"p","schedule_type":"once","schedule":"bad","timezone":"UTC"}`,
		`{"title":"x","prompt":"p","thread_id":"bad","schedule_type":"cron","schedule":"* * * * *","timezone":"UTC"}`,
	}
	for _, body := range badBodies {
		schedulerRawRequest(t, server.URL, http.MethodPost, "/api/scheduled-tasks", body, "", http.StatusBadRequest)
	}
	schedulerRequest[map[string]string](t, server.URL, http.MethodGet, "/api/scheduled-tasks/missing", nil, "", http.StatusNotFound)
	schedulerRequest[map[string]string](t, server.URL, http.MethodDelete, "/api/scheduled-tasks/missing", nil, "", http.StatusNotFound)
	schedulerRequest[map[string]string](t, server.URL, http.MethodPost, "/api/scheduled-tasks/missing/pause", nil, "", http.StatusNotFound)
	schedulerRequest[map[string]string](t, server.URL, http.MethodPost, "/api/scheduled-tasks/missing/trigger", nil, "", http.StatusNotFound)

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	missingThread, _ := domain.NewThread(time.Now())
	schedulerRequest[map[string]string](t, server.URL, http.MethodPost, "/api/scheduled-tasks", map[string]any{
		"title": "missing thread", "prompt": "prompt", "thread_id": missingThread.ID,
		"schedule_type": "once", "schedule": future.Format(time.RFC3339),
	}, "", http.StatusNotFound)
	created := schedulerRequest[scheduler.Task](t, server.URL, http.MethodPost, "/api/scheduled-tasks", map[string]any{
		"title": "valid", "prompt": "prompt", "schedule_type": "once", "schedule": future.Format(time.RFC3339),
	}, "", http.StatusCreated)
	schedulerRequest[map[string]string](t, server.URL, http.MethodPatch, "/api/scheduled-tasks/"+created.ID, map[string]any{}, "", http.StatusBadRequest)

	foreign := created
	foreign.ID = "foreign"
	foreign.UserID = "other"
	foreign.CreatedAt = future.Add(-time.Minute)
	foreign.UpdatedAt = foreign.CreatedAt
	if err = service.scheduled.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	schedulerRequest[map[string]string](t, server.URL, http.MethodGet, "/api/scheduled-tasks/"+foreign.ID, nil, "", http.StatusNotFound)
	schedulerRequest[[]scheduler.Task](t, server.URL, http.MethodGet, "/api/scheduled-tasks?thread_id=none", nil, "", http.StatusOK)

	existingThread, _ := domain.NewThread(time.Now())
	if err = service.store.CreateThread(context.Background(), existingThread); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ensureScheduledThread(context.Background(), scheduler.Task{ThreadID: string(existingThread.ID)})
	if err != nil || resolved != existingThread.ID {
		t.Fatalf("ensureScheduledThread(existing) = %q, %v", resolved, err)
	}
	if _, err = service.ensureScheduledThread(context.Background(), scheduler.Task{ThreadID: "bad"}); err == nil {
		t.Fatal("ensureScheduledThread(invalid) error = nil")
	}
}

func TestScheduledTaskSQLitePersistenceAndBackgroundShutdown(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	directory := t.TempDir()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Storage = config.StorageConfig{Driver: "sqlite", DSN: filepath.Join(directory, "gofer.db")}
	cfg.Workspace.Root = filepath.Join(directory, "workspaces")
	cfg.Scheduler.Enabled = true
	cfg.Scheduler.PollIntervalSeconds = 1
	cfg.Scheduler.LeaseDurationSeconds = 1

	first, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	task := scheduler.Task{ID: "persistent", UserID: "local", Title: "persistent", Prompt: "prompt", ScheduleType: scheduler.Once, Schedule: future.Format(time.RFC3339), Timezone: "UTC", Status: scheduler.Enabled, NextRunAt: future, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = first.scheduled.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Scheduler.Enabled = false
	second, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	stored, err := second.scheduled.Get(context.Background(), task.ID)
	if err != nil || stored.ID != task.ID {
		t.Fatalf("persisted = %#v, %v", stored, err)
	}
}

func schedulerRequest[T any](t *testing.T, baseURL, method, path string, body any, authorization string, wantStatus int) T {
	t.Helper()
	var raw string
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(encoded)
	}
	response := schedulerRawRequest(t, baseURL, method, path, raw, authorization, wantStatus)
	var value T
	if wantStatus != http.StatusNoContent {
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
	}
	_ = response.Body.Close()
	return value
}

func schedulerRawRequest(t *testing.T, baseURL, method, path, body, authorization string, wantStatus int) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", authorization)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("%s %s status = %d, want %d: %s", method, path, response.StatusCode, wantStatus, payload)
	}
	return response
}
