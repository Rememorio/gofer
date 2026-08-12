package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestServiceRequiresCurrentReadBeforeExistingFileWrite(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var sawBlocked atomic.Bool
	var sawRevision atomic.Bool
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		turn := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		switch turn {
		case 1:
			writeSSE(writer,
				toolCallChunk("blind", "write_file", `{"path":"/mnt/user-data/workspace/report.md","content":"blind"}`),
				doneChunk("tool_calls"),
			)
		case 2:
			sawBlocked.Store(bytes.Contains(body, []byte("read_before_write")))
			writeSSE(writer,
				toolCallChunk("read", "read_file", `{"path":"/mnt/user-data/workspace/report.md"}`),
				doneChunk("tool_calls"),
			)
		case 3:
			sawRevision.Store(bytes.Contains(body, []byte("revision")))
			writeSSE(writer,
				toolCallChunk("informed", "write_file", `{"path":"/mnt/user-data/workspace/report.md","content":"updated"}`),
				doneChunk("tool_calls"),
			)
		default:
			writeSSE(writer, textChunk("done"), doneChunk("stop"))
		}
	}))
	defer modelServer.Close()

	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")
	threadWorkspace, err := service.workspaces.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err = threadWorkspace.WriteFile(workspace.WorkspaceRoot+"/report.md", []byte("original"), false); err != nil {
		t.Fatal(err)
	}
	_ = threadWorkspace.Close()

	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"update the report"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	if calls.Load() != 4 || !sawBlocked.Load() || !sawRevision.Load() {
		t.Fatalf("model calls=%d blocked=%v revision=%v", calls.Load(), sawBlocked.Load(), sawRevision.Load())
	}
	threadWorkspace, err = service.workspaces.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = threadWorkspace.Close() }()
	result, err := threadWorkspace.ReadFile(workspace.WorkspaceRoot+"/report.md", workspace.ReadOptions{})
	if err != nil || result.Content != "updated" {
		t.Fatalf("final file = %#v, %v", result, err)
	}
}
