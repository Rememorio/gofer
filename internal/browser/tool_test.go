package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestBrowserToolsExecuteStatefulActions(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t)
	wantUntrusted := []string{
		"browser_back", "browser_click", "browser_navigate",
		"browser_scroll", "browser_snapshot", "browser_type",
	}
	if got := fixture.registry.UntrustedOutputTools(); !reflect.DeepEqual(got, wantUntrusted) {
		t.Fatalf("untrusted output tools = %#v, want %#v", got, wantUntrusted)
	}
	calls := []struct {
		name      string
		arguments string
		wantCall  string
	}{
		{name: "browser_navigate", arguments: `{"url":"https://example.com"}`, wantCall: "navigate"},
		{name: "browser_snapshot", arguments: `{}`, wantCall: "snapshot"},
		{name: "browser_click", arguments: `{"ref":1}`, wantCall: "click:1"},
		{name: "browser_type", arguments: `{"ref":1,"text":"hello","submit":true}`, wantCall: "type:1:hello:true"},
		{name: "browser_scroll", arguments: `{"delta_x":1,"delta_y":20}`, wantCall: "scroll:1:20"},
		{name: "browser_back", arguments: `{}`, wantCall: "back"},
	}
	for index, call := range calls {
		result, err := fixture.registry.Execute(context.Background(), domain.ToolCall{
			ID: strconv.Itoa(index), Name: call.name,
			Arguments: json.RawMessage(call.arguments),
		})
		if err != nil || result.IsError || !strings.Contains(string(result.Output), "untrusted data") {
			t.Fatalf("Execute(%s) = %#v, %v", call.name, result, err)
		}
		if got := fixture.session.calls[len(fixture.session.calls)-1]; got != call.wantCall {
			t.Fatalf("session call = %q, want %q", got, call.wantCall)
		}
	}
	status := fixture.manager.Status()
	if len(status) != 1 || status[0].Pinned != 0 {
		t.Fatalf("manager Status() = %#v", status)
	}
}

func TestBrowserScreenshotCreatesArtifact(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t)
	result, err := fixture.registry.Execute(context.Background(), domain.ToolCall{
		ID: "shot", Name: "browser_screenshot",
		Arguments: json.RawMessage(`{"filename":"evidence.png","full_page":true}`),
	})
	if err != nil || result.IsError || !fixture.session.fullPage {
		t.Fatalf("Execute(screenshot) = %#v, %v", result, err)
	}
	var output struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}
	if !strings.HasPrefix(output.Path, workspace.OutputsRoot+"/evidence-") || !strings.HasSuffix(output.Path, ".png") {
		t.Fatalf("screenshot path = %q", output.Path)
	}
	entry, err := fixture.workspace.Inspect(output.Path)
	if err != nil || entry.Size != int64(len(fixture.session.image)) {
		t.Fatalf("Inspect() = %#v, %v", entry, err)
	}
	artifacts := fixture.artifacts.List(fixture.workspace.ID())
	if len(artifacts) != 1 || artifacts[0].Path != output.Path || artifacts[0].MediaType != "image/png" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestBrowserCloseToolIsIdempotentAtToolBoundary(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t)
	result, err := fixture.registry.Execute(context.Background(), domain.ToolCall{
		ID: "close-1", Name: "browser_close", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.IsError || string(result.Output) != `{"closed":false}` {
		t.Fatalf("Execute(close absent) = %#v, %v", result, err)
	}
	if _, err := fixture.registry.Execute(context.Background(), domain.ToolCall{
		ID: "open", Name: "browser_snapshot", Arguments: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Execute(snapshot): %v", err)
	}
	result, err = fixture.registry.Execute(context.Background(), domain.ToolCall{
		ID: "close-2", Name: "browser_close", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.IsError || string(result.Output) != `{"closed":true}` || fixture.session.closeCalls != 1 {
		t.Fatalf("Execute(close) = %#v, closeCalls=%d, err=%v", result, fixture.session.closeCalls, err)
	}
}

func TestBrowserToolsValidationAndPolicies(t *testing.T) {
	t.Parallel()

	if err := (Tools{}).Register(tool.NewRegistry()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Register(invalid) error = %v", err)
	}
	fixture := newToolFixture(t)
	if err := fixture.tools.Register(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Register(nil registry) error = %v", err)
	}
	if _, err := fixture.tools.writeScreenshot("../bad", []byte("png")); err == nil {
		t.Fatal("writeScreenshot(unsafe) error = nil")
	}
	fixture.tools.Random = errorReader{}
	if _, err := fixture.tools.writeScreenshot("safe", []byte("png")); err == nil {
		t.Fatal("writeScreenshot(random error) error = nil")
	}
	descriptors := PolicyDescriptors()
	if descriptors["browser_navigate"].Effect != policy.EffectNetwork ||
		!reflect.DeepEqual(descriptors["browser_navigate"].ResourceFields, []string{"url"}) ||
		descriptors["browser_screenshot"].Effect != policy.EffectWrite {
		t.Fatalf("PolicyDescriptors() = %#v", descriptors)
	}
	if err := decodeArguments(json.RawMessage(`{} {}`), &struct{}{}); err == nil {
		t.Fatal("decodeArguments(multiple) error = nil")
	}
}

type toolFixture struct {
	manager   *Manager
	registry  *tool.Registry
	session   *toolSession
	workspace *workspace.Thread
	artifacts *artifact.Catalog
	tools     Tools
}

func newToolFixture(t *testing.T) toolFixture {
	t.Helper()
	workspaceManager, err := workspace.NewManager(workspace.Config{Root: t.TempDir(), MaxUploadBytes: 1 << 20})
	if err != nil {
		t.Fatalf("workspace.NewManager(): %v", err)
	}
	threadID, err := domain.NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}
	threadWorkspace, err := workspaceManager.Open(threadID)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() { _ = threadWorkspace.Close() })
	session := &toolSession{snapshot: Snapshot{
		URL: "https://example.com", Title: "Example", Elements: []Element{{Ref: 1, Tag: "button", Name: "Go"}},
	}, image: []byte("\x89PNG\r\n\x1a\nimage")}
	manager := newTestManager(t, ManagerConfig{Factory: func(context.Context, string) (Session, error) {
		return session, nil
	}})
	artifacts := artifact.NewCatalog()
	browserTools := Tools{
		Manager: manager, Key: string(threadID), Workspace: threadWorkspace, Artifacts: artifacts,
		Now:    func() time.Time { return time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC) },
		Random: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 100)),
	}
	registry := tool.NewRegistry()
	if err := browserTools.Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	return toolFixture{
		manager: manager, registry: registry, session: session,
		workspace: threadWorkspace, artifacts: artifacts, tools: browserTools,
	}
}

type toolSession struct {
	snapshot   Snapshot
	image      []byte
	calls      []string
	fullPage   bool
	closeCalls int
}

func (session *toolSession) Navigate(_ context.Context, rawURL string) (Snapshot, error) {
	session.calls = append(session.calls, "navigate")
	session.snapshot.URL = rawURL
	return session.snapshot, nil
}
func (session *toolSession) Snapshot(context.Context) (Snapshot, error) {
	session.calls = append(session.calls, "snapshot")
	return session.snapshot, nil
}
func (session *toolSession) Click(_ context.Context, ref int) (Snapshot, error) {
	session.calls = append(session.calls, "click:"+strconv.Itoa(ref))
	return session.snapshot, nil
}
func (session *toolSession) Type(_ context.Context, ref int, text string, submit bool) (Snapshot, error) {
	session.calls = append(session.calls, strings.Join([]string{
		"type", strconv.Itoa(ref), text, jsonBool(submit),
	}, ":"))
	return session.snapshot, nil
}
func (session *toolSession) Scroll(_ context.Context, deltaX, deltaY int) (Snapshot, error) {
	session.calls = append(session.calls, "scroll:"+jsonInt(deltaX)+":"+jsonInt(deltaY))
	return session.snapshot, nil
}
func (session *toolSession) Back(context.Context) (Snapshot, error) {
	session.calls = append(session.calls, "back")
	return session.snapshot, nil
}
func (session *toolSession) Screenshot(_ context.Context, fullPage bool) ([]byte, error) {
	session.fullPage = fullPage
	return append([]byte(nil), session.image...), nil
}
func (session *toolSession) Close() error {
	session.closeCalls++
	return nil
}

func jsonBool(value bool) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func jsonInt(value int) string {
	data, _ := json.Marshal(value)
	return string(data)
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
