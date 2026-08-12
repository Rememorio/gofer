package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/gateway"
)

func TestServiceAssemblesConfiguredWebResearchTools(t *testing.T) {
	t.Parallel()

	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Web.Search.Enabled = true
	cfg.Web.Search.Provider = "searxng"
	cfg.Web.Search.Endpoint = modelServer.URL
	cfg.Web.Search.AllowPrivateAddresses = true
	cfg.Web.Fetch.Enabled = true
	cfg.Web.Fetch.AllowPrivateAddresses = true
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer func() { _ = service.Close() }()
	if service.research == nil || !service.research.HasSearch() || !service.research.HasFetch() {
		t.Fatalf("research client = %#v", service.research)
	}
	thread, _ := domain.NewThread(time.Now())
	threadWorkspace, err := service.workspaces.Open(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = threadWorkspace.Close() }()
	run, _ := domain.NewRun(thread.ID, time.Now())
	registry, _, children, err := service.buildTools(
		threadWorkspace,
		gateway.StartRequest{RunID: run.ID, ThreadID: thread.ID},
		service.providers["primary"],
	)
	if err != nil {
		t.Fatalf("buildTools(): %v", err)
	}
	defer func() { _ = children.Close() }()
	names := make(map[string]bool)
	for _, definition := range registry.Definitions() {
		names[definition.Name] = true
	}
	if !names["web_search"] || !names["web_fetch"] {
		t.Fatalf("tool names = %#v", names)
	}

	server := httptest.NewServer(service.Handler())
	defer server.Close()
	features := resourceRequest[map[string]map[string]bool](t, server.URL, http.MethodGet, "/api/features", nil, "", http.StatusOK)
	if !features["web_search"]["enabled"] || !features["web_fetch"]["enabled"] {
		t.Fatalf("features = %#v", features)
	}
}
