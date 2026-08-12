package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/delivery"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

func TestRunFailsAfterReceiptWhenProducedOutputIsNotPresented(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	service, server := newDeliveryTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeSSE(writer,
				toolCallChunk("write-1", "write_file", `{"path":"/mnt/user-data/outputs/report.md","content":"ready"}`),
				doneChunk("tool_calls"),
			)
			return
		}
		writeSSE(writer, textChunk("done"), doneChunk("stop"))
	})
	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"make report"}]}}`, "")
	run := waitRun(t, server.URL, threadID, runID, domain.RunFailed, "")
	if !strings.Contains(strings.ToLower(run.Error), "artifact delivery incomplete") {
		t.Fatalf("run error = %q", run.Error)
	}
	receipt, kinds := runDeliveryReceipt(t, service, runID)
	if receipt.Verdict == nil || receipt.Satisfied || receipt.Stage != "not_started" || len(receipt.ProducedPaths) != 1 {
		t.Fatalf("receipt = %#v", receipt)
	}
	assertOrderedKinds(t, kinds, event.WorkspaceChanges, event.RunDelivery, event.RunFailed)
}

func TestRunSucceedsWhenPresentationMatchesProducedOutput(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	service, server := newDeliveryTestService(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			writeSSE(writer,
				toolCallChunk("write-1", "write_file", `{"path":"/mnt/user-data/outputs/report.md","content":"ready"}`),
				doneChunk("tool_calls"),
			)
		case 2:
			writeSSE(writer,
				toolCallChunk("present-1", delivery.ToolPresentFiles, `{"filepaths":["/mnt/user-data/outputs/report.md"]}`),
				doneChunk("tool_calls"),
			)
		default:
			writeSSE(writer, textChunk("delivered"), doneChunk("stop"))
		}
	})
	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"make report"}]}}`, "")
	run := waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	if run.Error != "" || calls.Load() != 3 {
		t.Fatalf("run/calls = %#v/%d", run, calls.Load())
	}
	receipt, kinds := runDeliveryReceipt(t, service, runID)
	if receipt.Verdict == nil || !receipt.Satisfied || receipt.Stage != "presented" ||
		len(receipt.PresentedPaths) != 1 || len(receipt.MatchedPaths) != 1 {
		t.Fatalf("receipt = %#v", receipt)
	}
	assertOrderedKinds(t, kinds, event.WorkspaceChanges, event.RunDelivery, event.RunCompleted)
}

func newDeliveryTestService(t *testing.T, modelHandler http.HandlerFunc) (*Service, *httptest.Server) {
	t.Helper()
	modelServer := httptest.NewServer(modelHandler)
	t.Cleanup(modelServer.Close)
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	return service, server
}

func runDeliveryReceipt(t *testing.T, service *Service, runID domain.RunID) (delivery.Receipt, []event.Kind) {
	t.Helper()
	records, err := service.store.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]event.Kind, len(records))
	var receipt delivery.Receipt
	found := false
	for index, record := range records {
		kinds[index] = record.Kind
		if record.Kind == event.RunDelivery {
			var payload delivery.EventPayload
			if err = event.Decode(record, &payload); err != nil {
				t.Fatal(err)
			}
			receipt, found = payload.Content, true
		}
	}
	if !found {
		t.Fatal("run.delivery event not found")
	}
	return receipt, kinds
}

func assertOrderedKinds(t *testing.T, kinds []event.Kind, ordered ...event.Kind) {
	t.Helper()
	next := 0
	for _, kind := range kinds {
		if next < len(ordered) && kind == ordered[next] {
			next++
		}
	}
	if next != len(ordered) {
		t.Fatalf("event kinds = %#v, want ordered %#v", kinds, ordered)
	}
}

func containsKind(kinds []event.Kind, want event.Kind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func toolCallChunk(id, name, arguments string) string {
	return `{"id":"call","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + quotedJSON(arguments) + `}}]},"finish_reason":null}]}`
}

func quotedJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func textChunk(text string) string {
	return `{"id":"call","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":` + quotedJSON(text) + `},"finish_reason":null}]}`
}

func doneChunk(reason string) string {
	return `{"id":"call","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":` + quotedJSON(reason) + `}]}`
}
