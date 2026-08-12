package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestBranchLatestTurnClonesHistoryAndWorkspace(t *testing.T) {
	t.Parallel()
	service, server := newBranchTestService(t)
	sourceID := createThread(t, server.URL, "")
	messages := branchTestMessages(t)
	if _, err := conversation.Seed(context.Background(), service.store, sourceID, messages, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sourceWorkspace, err := service.workspaces.Open(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = sourceWorkspace.WriteFile(workspace.WorkspaceRoot+"/notes.txt", []byte("branch me"), false); err != nil {
		t.Fatal(err)
	}
	_ = sourceWorkspace.Close()

	response := resourceRequest[branchResponse](t, server.URL, http.MethodPost,
		"/api/threads/"+string(sourceID)+"/branches",
		branchInput{MessageID: string(messages[3].ID), Title: "Focused branch"}, "", http.StatusOK)
	if response.ParentThreadID != sourceID || response.BranchedFromMessage != messages[3].ID || response.WorkspaceCloneMode != "current_thread_best_effort" || response.HistorySeedMode != "seeded" {
		t.Fatalf("branch response = %#v", response)
	}
	branchedMessages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(response.ThreadID)+"/messages", nil, "", http.StatusOK)
	if len(branchedMessages) != len(messages) || branchedMessages[3].ID != messages[3].ID {
		t.Fatalf("branched messages = %#v", branchedMessages)
	}
	branch, err := service.store.Thread(context.Background(), response.ThreadID)
	if err != nil || branch.Title != "Focused branch" || branch.Metadata["branch_parent_thread_id"] != string(sourceID) || branch.Metadata["branch_parent_message_id"] != string(messages[3].ID) {
		t.Fatalf("branch thread = %#v, %v", branch, err)
	}
	branchWorkspace, err := service.workspaces.Open(response.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = branchWorkspace.Close() }()
	file, err := branchWorkspace.ReadFile(workspace.WorkspaceRoot+"/notes.txt", workspace.ReadOptions{})
	if err != nil || file.Content != "branch me" {
		t.Fatalf("branch workspace file = %#v, %v", file, err)
	}
}

func TestBranchHistoricalTurnSkipsWorkspaceClone(t *testing.T) {
	t.Parallel()
	service, server := newBranchTestService(t)
	sourceID := createThread(t, server.URL, "")
	messages := branchTestMessages(t)
	if _, err := conversation.Seed(context.Background(), service.store, sourceID, messages, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sourceWorkspace, _ := service.workspaces.Open(sourceID)
	_ = sourceWorkspace.WriteFile(workspace.WorkspaceRoot+"/future.txt", []byte("newer state"), false)
	_ = sourceWorkspace.Close()

	response := resourceRequest[branchResponse](t, server.URL, http.MethodPost,
		"/api/threads/"+string(sourceID)+"/branches",
		branchInput{MessageID: string(messages[1].ID), MessageIDs: []string{string(messages[1].ID)}}, "", http.StatusOK)
	if response.WorkspaceCloneMode != "skipped_historical_turn" {
		t.Fatalf("workspace clone mode = %q", response.WorkspaceCloneMode)
	}
	branchedMessages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(response.ThreadID)+"/messages", nil, "", http.StatusOK)
	if len(branchedMessages) != 2 || branchedMessages[1].ID != messages[1].ID {
		t.Fatalf("branched messages = %#v", branchedMessages)
	}
	branchWorkspace, _ := service.workspaces.Open(response.ThreadID)
	defer func() { _ = branchWorkspace.Close() }()
	if _, err := branchWorkspace.Inspect(workspace.WorkspaceRoot + "/future.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect(future file) = %v, want not found", err)
	}
}

func TestBranchMissingWorkspaceAndValidation(t *testing.T) {
	t.Parallel()
	service, server := newBranchTestService(t)
	sourceID := createThread(t, server.URL, "")
	messages := branchTestMessages(t)
	if _, err := conversation.Seed(context.Background(), service.store, sourceID, messages, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	path := "/api/threads/" + string(sourceID) + "/branches"
	response := resourceRequest[branchResponse](t, server.URL, http.MethodPost, path,
		branchInput{MessageID: string(messages[3].ID)}, "", http.StatusOK)
	if response.WorkspaceCloneMode != "not_found" {
		t.Fatalf("workspace clone mode = %q", response.WorkspaceCloneMode)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{}, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{MessageID: "bad"}, "", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{MessageID: string(messages[0].ID)}, "", http.StatusConflict)
	missingMessage, _ := domain.NewTextMessage(domain.RoleAssistant, "missing", time.Now())
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{MessageID: string(missingMessage.ID)}, "", http.StatusConflict)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, strings.NewReader(`{"message_id":"`+string(messages[3].ID)+`","unknown":true}`), "application/json", http.StatusUnprocessableEntity)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, strings.NewReader(`{}`+`{}`), "application/json", http.StatusUnprocessableEntity)
	missingThread, _ := domain.NewThreadID()
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/threads/"+string(missingThread)+"/branches", branchInput{MessageID: string(messages[3].ID)}, "", http.StatusNotFound)
	before, err := service.store.Threads(context.Background(), store.ThreadQuery{OwnerID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	sourceWorkspace, err := service.workspaces.Open(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	mount := sourceWorkspace.ExecutionMounts()[0]
	_ = sourceWorkspace.Close()
	if err = os.Symlink(t.TempDir(), filepath.Join(mount.HostPath, "unsafe-link")); err == nil {
		resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{MessageID: string(messages[3].ID)}, "", http.StatusInternalServerError)
		after, listErr := service.store.Threads(context.Background(), store.ThreadQuery{OwnerID: "local"})
		if listErr != nil || len(after) != len(before) {
			t.Fatalf("failed branch left target: before=%d after=%d err=%v", len(before), len(after), listErr)
		}
	}

	run, _ := domain.NewRun(sourceID, time.Now())
	if err := service.store.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, path, branchInput{MessageID: string(messages[3].ID)}, "", http.StatusConflict)
}

func TestBranchSelectionAndTitles(t *testing.T) {
	t.Parallel()
	messages := branchTestMessages(t)
	seed, target, latest, err := selectBranchHistory(messages, branchInput{MessageID: string(messages[3].ID), MessageIDs: []string{string(messages[1].ID), string(messages[3].ID)}})
	if err != nil || !latest || len(seed) != 4 || target.ID != messages[3].ID {
		t.Fatalf("selectBranchHistory() = %#v, %#v, %v, %v", seed, target, latest, err)
	}
	source, _ := domain.NewThread(time.Now())
	branch, err := newBranchThread(source, messages[1].ID, "", time.Now())
	if err != nil || branch.Title != "Branch" {
		t.Fatalf("newBranchThread(empty title) = %#v, %v", branch, err)
	}
	source.Title = "Research"
	branch, err = newBranchThread(source, messages[1].ID, "", time.Now())
	if err != nil || branch.Title != "Research (branch)" {
		t.Fatalf("newBranchThread(default title) = %#v, %v", branch, err)
	}
	if _, err = newBranchThread(source, messages[1].ID, strings.Repeat("x", 257), time.Now()); !errors.Is(err, errInvalidBranch) {
		t.Fatalf("newBranchThread(long title) = %v", err)
	}
	if mode, err := (&Service{}).cloneBranchWorkspace(source.ID, branch.ID, false); err != nil || mode != "skipped_historical_turn" {
		t.Fatalf("cloneBranchWorkspace(historical) = %q, %v", mode, err)
	}
}

func newBranchTestService(t *testing.T) (*Service, *httptest.Server) {
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

func branchTestMessages(t *testing.T) []domain.Message {
	t.Helper()
	now := time.Now().Add(-time.Hour).UTC()
	roles := []domain.Role{domain.RoleUser, domain.RoleAssistant, domain.RoleUser, domain.RoleAssistant}
	texts := []string{"first question", "first answer", "second question", "second answer"}
	messages := make([]domain.Message, len(roles))
	for index := range roles {
		message, err := domain.NewTextMessage(roles[index], texts[index], now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		messages[index] = message
	}
	return messages
}
