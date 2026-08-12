package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/feedback"
)

func TestFeedbackHTTPWorkflow(t *testing.T) {
	t.Parallel()
	service, server := newFeedbackTestService(t)
	threadID := createThread(t, server.URL, "")
	run, _ := domain.NewRun(threadID, time.Now())
	if err := service.store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	messageID, _ := domain.NewMessageID()
	path := "/api/threads/" + string(threadID) + "/runs/" + string(run.ID) + "/feedback"
	created := resourceRequest[feedback.Entry](t, server.URL, http.MethodPost, path,
		feedbackInput{Rating: 1, Comment: "helpful", MessageID: string(messageID)}, "", http.StatusOK)
	if created.Rating != 1 || created.MessageID != string(messageID) || created.UserID != "local" {
		t.Fatalf("created feedback = %#v", created)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path,
		feedbackInput{Rating: -1}, "", http.StatusConflict)
	listed := resourceRequest[[]feedback.Entry](t, server.URL, http.MethodGet, path, nil, "", http.StatusOK)
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed feedback = %#v", listed)
	}
	global := resourceRequest[[]feedback.Entry](t, server.URL, http.MethodGet,
		"/api/runs/"+string(run.ID)+"/feedback", nil, "", http.StatusOK)
	if len(global) != 1 || global[0].ID != created.ID {
		t.Fatalf("global feedback = %#v", global)
	}
	stats := resourceRequest[feedback.Stats](t, server.URL, http.MethodGet, path+"/stats", nil, "", http.StatusOK)
	if stats.Total != 1 || stats.Positive != 1 || stats.Negative != 0 {
		t.Fatalf("feedback stats = %#v", stats)
	}
	updated := resourceRequest[feedback.Entry](t, server.URL, http.MethodPut, path,
		map[string]any{"rating": -1, "comment": "incorrect"}, "", http.StatusOK)
	if updated.ID != created.ID || updated.Rating != -1 || updated.MessageID != "" {
		t.Fatalf("updated feedback = %#v", updated)
	}
	missingID, _ := domain.NewFeedbackID()
	resourceRequest[map[string]string](t, server.URL, http.MethodDelete, path+"/"+string(missingID), nil, "", http.StatusNotFound)
	resourceRequest[map[string]bool](t, server.URL, http.MethodDelete, path+"/"+string(updated.ID), nil, "", http.StatusOK)
	resourceRequest[map[string]string](t, server.URL, http.MethodDelete, path, nil, "", http.StatusNotFound)
	notFound := resourceRawRequest(t, server.URL, http.MethodGet, path+"/bad", nil, "", http.StatusNotFound)
	_ = notFound.Body.Close()
}

func TestFeedbackHTTPValidationAndRunScoping(t *testing.T) {
	t.Parallel()
	service, server := newFeedbackTestService(t)
	firstID := createThread(t, server.URL, "")
	secondID := createThread(t, server.URL, "")
	run, _ := domain.NewRun(firstID, time.Now())
	if err := service.store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	path := "/api/threads/" + string(firstID) + "/runs/" + string(run.ID) + "/feedback"
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, feedbackInput{Rating: 0}, "", http.StatusBadRequest)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, feedbackInput{Rating: 1, MessageID: "bad"}, "", http.StatusBadRequest)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path,
		strings.NewReader(`{"rating":1,"unknown":true}`), "application/json", http.StatusBadRequest)
	resourceRequest[map[string]string](t, server.URL, http.MethodPut, path,
		strings.NewReader(`{"rating":1}{}`), "application/json", http.StatusBadRequest)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet,
		"/api/threads/"+string(secondID)+"/runs/"+string(run.ID)+"/feedback", nil, "", http.StatusNotFound)
	missingRun, _ := domain.NewRunID()
	resourceRequest[map[string]string](t, server.URL, http.MethodGet,
		"/api/threads/"+string(firstID)+"/runs/"+string(missingRun)+"/feedback", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet,
		"/api/runs/"+string(missingRun)+"/feedback", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet,
		"/api/threads/bad/runs/"+string(run.ID)+"/feedback", nil, "", http.StatusBadRequest)
}

func TestSQLiteFeedbackWiresIntoServiceRestart(t *testing.T) {
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
	run, _ := domain.NewRun(threadID, time.Now())
	if err = first.store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	path := "/api/threads/" + string(threadID) + "/runs/" + string(run.ID) + "/feedback"
	created := resourceRequest[feedback.Entry](t, server.URL, http.MethodPut, path, map[string]any{"rating": 1, "comment": "durable"}, "", http.StatusOK)
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
	listed := resourceRequest[[]feedback.Entry](t, server.URL, http.MethodGet, path, nil, "", http.StatusOK)
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].Comment != "durable" {
		t.Fatalf("restarted feedback = %#v", listed)
	}
}

func TestFeedbackHTTPAuthorizationAndOwnerIsolation(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	aliceToken, bobToken, readerToken := "alice-feedback-token-000001", "bob-feedback-token-00000002", "reader-feedback-token-0003"
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Auth = config.AuthConfig{Enabled: true, Tokens: []config.AuthTokenConfig{
		{Secret: aliceToken, PrincipalID: "alice", Permissions: []string{string(auth.ThreadsRead), string(auth.ThreadsWrite), string(auth.ThreadsDelete), string(auth.RunsRead)}},
		{Secret: bobToken, PrincipalID: "bob", Permissions: []string{string(auth.ThreadsRead), string(auth.ThreadsWrite), string(auth.ThreadsDelete), string(auth.RunsRead)}},
		{Secret: readerToken, PrincipalID: "reader", Permissions: []string{string(auth.ThreadsRead), string(auth.RunsRead)}},
	}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "Bearer "+aliceToken)
	run, _ := domain.NewRun(threadID, time.Now())
	if err = service.store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	path := "/api/threads/" + string(threadID) + "/runs/" + string(run.ID) + "/feedback"
	created := authorizedMemoryRequest[feedback.Entry](t, server.URL, http.MethodPut, path, map[string]int{"rating": 1}, aliceToken, http.StatusOK)
	if created.UserID != "alice" {
		t.Fatalf("feedback owner = %q", created.UserID)
	}
	authorizedMemoryRequest[map[string]string](t, server.URL, http.MethodGet, path, nil, bobToken, http.StatusNotFound)
	authorizedMemoryRequest[map[string]string](t, server.URL, http.MethodPut, path, map[string]int{"rating": -1}, readerToken, http.StatusForbidden)
	authorizedMemoryRequest[map[string]string](t, server.URL, http.MethodGet, "/api/runs/"+string(run.ID)+"/feedback", nil, bobToken, http.StatusNotFound)
}

func newFeedbackTestService(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(modelServer.Close)
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	return service, server
}
