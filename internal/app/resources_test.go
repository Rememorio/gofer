package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/skill"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestResourceDiscoveryAndSkillManagement(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Models[0].Options = map[string]any{"display_name": "Test Model", "supports_thinking": true, "supports_vision": false}
	skillRoot := t.TempDir()
	skillDirectory := filepath.Join(skillRoot, "public", "demo")
	if err := os.MkdirAll(skillDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	document := "---\nname: demo\ndescription: Demo workflow\n---\n# Demo\nFollow this workflow.\n"
	if err := os.WriteFile(filepath.Join(skillDirectory, "SKILL.md"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Skills.Enabled = true
	cfg.Skills.Root = skillRoot
	cfg.Skills.ProjectionRoot = filepath.Join(t.TempDir(), "projection")
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()

	models := resourceRequest[struct {
		Models []modelResource `json:"models"`
	}](t, server.URL, http.MethodGet, "/api/models", nil, "", http.StatusOK)
	if len(models.Models) != 1 || models.Models[0].DisplayName != "Test Model" || !models.Models[0].SupportsThinking || models.Models[0].SupportsVision {
		t.Fatalf("models = %#v", models)
	}
	model := resourceRequest[modelResource](t, server.URL, http.MethodGet, "/api/models/primary", nil, "", http.StatusOK)
	if model.Model != "gpt-test" {
		t.Fatalf("model = %#v", model)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/models/missing", nil, "", http.StatusNotFound)
	features := resourceRequest[map[string]map[string]bool](t, server.URL, http.MethodGet, "/api/features", nil, "", http.StatusOK)
	assertResourceFeatures(t, features)
	skills := resourceRequest[struct {
		Skills []skill.Skill `json:"skills"`
	}](t, server.URL, http.MethodGet, "/api/skills", nil, "", http.StatusOK)
	if len(skills.Skills) != 1 || skills.Skills[0].Name != "demo" {
		t.Fatalf("skills = %#v", skills)
	}
	detail := resourceRequest[struct {
		skill.Skill
		Content string `json:"content"`
	}](t, server.URL, http.MethodGet, "/api/skills/demo", nil, "", http.StatusOK)
	if !detail.Enabled || !strings.Contains(detail.Content, "Follow this workflow") {
		t.Fatalf("skill detail = %#v", detail)
	}
	disabled := resourceRequest[map[string]any](t, server.URL, http.MethodPost, "/api/skills/demo/disable", nil, "", http.StatusOK)
	if disabled["enabled"] != false {
		t.Fatalf("disabled = %#v", disabled)
	}
	detail = resourceRequest[struct {
		skill.Skill
		Content string `json:"content"`
	}](t, server.URL, http.MethodGet, "/api/skills/demo", nil, "", http.StatusOK)
	if detail.Enabled || detail.Content != "" {
		t.Fatalf("disabled detail = %#v", detail)
	}
	resourceRequest[map[string]any](t, server.URL, http.MethodPost, "/api/skills/demo/enable", nil, "", http.StatusOK)
	resourceRequest[map[string]any](t, server.URL, http.MethodPost, "/api/skills/reload", nil, "", http.StatusOK)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/skills/missing", nil, "", http.StatusNotFound)
	originalProjection := service.config.Skills.ProjectionRoot
	service.config.Skills.ProjectionRoot = service.config.Skills.Root
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/skills/demo/disable", nil, "", http.StatusInternalServerError)
	service.config.Skills.ProjectionRoot = originalProjection
	detail = resourceRequest[struct {
		skill.Skill
		Content string `json:"content"`
	}](t, server.URL, http.MethodGet, "/api/skills/demo", nil, "", http.StatusOK)
	if !detail.Enabled {
		t.Fatal("failed projection did not roll skill state back")
	}
}

func assertResourceFeatures(t *testing.T, features map[string]map[string]bool) {
	t.Helper()
	if !features["skills"]["enabled"] || !features["memory"]["enabled"] ||
		!features["read_before_write"]["enabled"] || !features["loop_detection"]["enabled"] ||
		features["browser_control"]["enabled"] {
		t.Fatalf("features = %#v", features)
	}
}

func TestUploadAndArtifactHTTPWorkflow(t *testing.T) {
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
	limits := resourceRequest[map[string]int64](t, server.URL, http.MethodGet, "/api/threads/"+string(threadID)+"/uploads/limits", nil, "", http.StatusOK)
	if limits["max_file_size"] == 0 || limits["max_files"] != maxUploadFiles {
		t.Fatalf("limits = %#v", limits)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, content := range []string{"first", "second"} {
		part, createErr := form.CreateFormFile("files", "report.txt")
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, _ = io.WriteString(part, content)
	}
	_ = form.Close()
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/api/threads/"+string(threadID)+"/uploads", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var uploaded struct {
		Files []uploadResource `json:"files"`
	}
	if err = json.NewDecoder(response.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(uploaded.Files) != 2 || uploaded.Files[1].Filename != "report-1.txt" {
		t.Fatalf("uploaded = %d %#v", response.StatusCode, uploaded)
	}
	listed := resourceRequest[struct {
		Files []uploadResource `json:"files"`
		Count int              `json:"count"`
	}](t, server.URL, http.MethodGet, "/api/threads/"+string(threadID)+"/uploads/list", nil, "", http.StatusOK)
	if listed.Count != 2 || len(listed.Files) != 2 {
		t.Fatalf("listed = %#v", listed)
	}
	download := resourceRawRequest(t, server.URL, http.MethodGet, uploaded.Files[0].ArtifactURL, nil, "", http.StatusOK)
	content, _ := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if string(content) != "first" || !strings.HasPrefix(download.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("upload artifact = %q, %q", content, download.Header.Get("Content-Disposition"))
	}
	rangeRequest, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+uploaded.Files[0].ArtifactURL, nil)
	rangeRequest.Header.Set("Range", "bytes=1-3")
	ranged, err := http.DefaultClient.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	rangedContent, _ := io.ReadAll(ranged.Body)
	_ = ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent || string(rangedContent) != "irs" {
		t.Fatalf("range = %d %q", ranged.StatusCode, rangedContent)
	}
	exerciseOutputArtifact(t, service, server.URL, threadID)
	resourceRequest[map[string]any](t, server.URL, http.MethodDelete, "/api/threads/"+string(threadID)+"/uploads/report.txt", nil, "", http.StatusOK)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/threads/"+string(threadID)+"/artifacts/etc/passwd", nil, "", http.StatusBadRequest)
}

func exerciseOutputArtifact(t *testing.T, service *Service, baseURL string, threadID domain.ThreadID) {
	t.Helper()
	threadWorkspace, err := service.workspaces.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err = threadWorkspace.CreateOutput(workspace.OutputsRoot+"/page.html", []byte("<h1>safe download</h1>")); err != nil {
		t.Fatal(err)
	}
	_ = threadWorkspace.Close()
	artifacts := resourceRequest[struct {
		Artifacts []struct {
			Path string `json:"path"`
		} `json:"artifacts"`
	}](t, baseURL, http.MethodGet, "/api/threads/"+string(threadID)+"/artifacts", nil, "", http.StatusOK)
	if len(artifacts.Artifacts) != 1 || artifacts.Artifacts[0].Path != workspace.OutputsRoot+"/page.html" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	outputURL := "/api/threads/" + string(threadID) + "/artifacts/mnt/user-data/outputs/page.html"
	output := resourceRawRequest(t, baseURL, http.MethodGet, outputURL, nil, "", http.StatusOK)
	_ = output.Body.Close()
	if !strings.HasPrefix(output.Header.Get("Content-Disposition"), "attachment") || output.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("output headers = %#v", output.Header)
	}
}

func TestResourceAPIErrorsAndDisabledSkills(t *testing.T) {
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
	resourceRequest[struct {
		Skills []skill.Skill `json:"skills"`
	}](t, server.URL, http.MethodGet, "/api/skills", nil, "", http.StatusOK)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/skills/reload", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/skills/demo/enable", nil, "", http.StatusNotFound)
	missingThread, _ := domain.NewThreadID()
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/threads/"+string(missingThread)+"/uploads/list", nil, "", http.StatusNotFound)
	threadID := createThread(t, server.URL, "")
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/uploads", strings.NewReader("not multipart"), "text/plain", http.StatusBadRequest)
	resourceRequest[map[string]string](t, server.URL, http.MethodDelete, "/api/threads/"+string(threadID)+"/uploads/missing.txt", nil, "", http.StatusNotFound)
	if !activeContent("text/html; charset=utf-8") || activeContent("text/plain") {
		t.Fatal("active content classification")
	}
	if uploadRequestLimit(1) != int64(1<<20)+maxUploadFiles || uploadRequestLimit(int64(^uint64(0)>>1)) != int64(^uint64(0)>>1) {
		t.Fatal("upload request limit")
	}
}

func TestReceiveUploadsIsAtomicAndBounded(t *testing.T) {
	t.Parallel()
	manager, err := workspace.NewManager(workspace.Config{Root: t.TempDir(), MaxUploadBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	threadID, _ := domain.NewThreadID()
	threadWorkspace, err := manager.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = threadWorkspace.Close() }()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, name := range []string{"one.txt", "two.txt"} {
		part, createErr := form.CreateFormFile("files", name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, _ = io.WriteString(part, name)
	}
	_ = form.Close()
	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), form.Boundary())
	if _, err = receiveUploads(threadWorkspace, reader, 1); !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("receiveUploads() = %v", err)
	}
	listed, err := threadWorkspace.List(workspace.UploadsRoot, workspace.ListOptions{MaxDepth: 1})
	if err != nil || len(listed.Entries) != 0 {
		t.Fatalf("partial uploads remain = %#v, %v", listed, err)
	}
}

func TestSQLiteSkillStateWiresIntoServiceRestart(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	directory := t.TempDir()
	skillRoot := filepath.Join(directory, "skills")
	skillDirectory := filepath.Join(skillRoot, "public", "demo")
	if err := os.MkdirAll(skillDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDirectory, "SKILL.md"), []byte("---\nname: demo\ndescription: Demo\n---\n# Demo\nUse it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Storage = config.StorageConfig{Driver: "sqlite", DSN: filepath.Join(directory, "gofer.db")}
	cfg.Workspace.Root = filepath.Join(directory, "workspaces")
	cfg.Skills.Enabled = true
	cfg.Skills.Root = skillRoot
	cfg.Skills.ProjectionRoot = filepath.Join(directory, "projection")
	first, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(first.Handler())
	resourceRequest[map[string]any](t, server.URL, http.MethodPost, "/api/skills/demo/disable", nil, "", http.StatusOK)
	server.Close()
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	metadata, err := second.skills.Get("demo")
	if err != nil || metadata.Enabled {
		t.Fatalf("persisted skill state = %#v, %v", metadata, err)
	}
}

func resourceRequest[T any](t *testing.T, baseURL, method, path string, body any, contentType string, wantStatus int) T {
	t.Helper()
	response := resourceRawRequest(t, baseURL, method, path, body, contentType, wantStatus)
	defer func() { _ = response.Body.Close() }()
	var value T
	if response.ContentLength != 0 {
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return value
}

func resourceRawRequest(t *testing.T, baseURL, method, path string, body any, contentType string, wantStatus int) *http.Response {
	t.Helper()
	var reader io.Reader
	switch value := body.(type) {
	case nil:
	case io.Reader:
		reader = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
		if contentType == "" {
			contentType = "application/json"
		}
	}
	request, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
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
