package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestServiceExternalizesToolOutputWithoutCreatingDeliverable(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeSSE(writer,
				toolCallChunk("list-1", "ls", `{"path":"/mnt/user-data/workspace","max_depth":1,"max_results":100}`),
				doneChunk("tool_calls"),
			)
			return
		}
		writeSSE(writer, textChunk("done"), doneChunk("stop"))
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.ToolOutput.ExternalizeMinChars = 40
	cfg.ToolOutput.PreviewHeadChars = 20
	cfg.ToolOutput.PreviewTailChars = 10
	cfg.ToolOutput.StorageSubdir = "process-cache"
	service, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	threadID := createThread(t, server.URL, "")
	threadWorkspace, err := service.workspaces.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = threadWorkspace.Close() })
	for index := range 12 {
		filename := fmt.Sprintf("%s/item-%02d.txt", workspace.WorkspaceRoot, index)
		if err = threadWorkspace.WriteFile(filename, []byte(strings.Repeat("x", 20)), false); err != nil {
			t.Fatal(err)
		}
	}
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"list files"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	records, err := service.store.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	preview := ""
	for _, record := range records {
		if record.Kind != event.ToolCompleted {
			continue
		}
		var payload struct {
			Result domain.ToolResult `json:"result"`
		}
		if err = event.Decode(record, &payload); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(payload.Result.Output, &preview); err != nil {
			t.Fatalf("tool output was not a budget preview: %s", payload.Result.Output)
		}
	}
	if !strings.Contains(preview, workspace.OutputsRoot+"/process-cache/") || !strings.Contains(preview, "Use read_file") {
		t.Fatalf("preview = %q", preview)
	}
	receipt, kinds := runDeliveryReceipt(t, service, runID)
	if receipt.Verdict != nil || receipt.Presented != 0 || containsKind(kinds, event.WorkspaceChanges) {
		t.Fatalf("spill became deliverable: receipt=%#v kinds=%#v", receipt, kinds)
	}
	listed := resourceRequest[struct {
		Artifacts []json.RawMessage `json:"artifacts"`
	}](t, server.URL, http.MethodGet, "/api/threads/"+string(threadID)+"/artifacts", nil, "", http.StatusOK)
	if len(listed.Artifacts) != 0 {
		t.Fatalf("internal spill was publicly listed: %s", listed.Artifacts)
	}
}
