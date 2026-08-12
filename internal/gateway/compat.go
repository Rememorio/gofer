package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

func (handler *Handler) streamRun(writer http.ResponseWriter, request *http.Request) {
	threadID, input, err := handler.decodeThreadRun(request, writer)
	if err != nil {
		writeCompatError(writer, err)
		return
	}
	run, err := handler.startRun(request.Context(), threadID, input)
	if err != nil {
		writeError(writer, err)
		return
	}
	handler.streamStartedRun(writer, request, run)
}

func (handler *Handler) waitRun(writer http.ResponseWriter, request *http.Request) {
	threadID, input, err := handler.decodeThreadRun(request, writer)
	if err != nil {
		writeCompatError(writer, err)
		return
	}
	run, err := handler.startRun(request.Context(), threadID, input)
	if err == nil {
		run, err = handler.waitForTerminal(request.Context(), run.ID)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	handler.writeWaitResult(writer, request, run)
}

func (handler *Handler) statelessStreamRun(writer http.ResponseWriter, request *http.Request) {
	threadID, input, err := handler.decodeStatelessRun(request, writer)
	if err != nil {
		writeCompatError(writer, err)
		return
	}
	run, err := handler.startRun(request.Context(), threadID, input)
	if err != nil {
		writeError(writer, err)
		return
	}
	handler.streamStartedRun(writer, request, run)
}

func (handler *Handler) statelessWaitRun(writer http.ResponseWriter, request *http.Request) {
	threadID, input, err := handler.decodeStatelessRun(request, writer)
	if err != nil {
		writeCompatError(writer, err)
		return
	}
	run, err := handler.startRun(request.Context(), threadID, input)
	if err == nil {
		run, err = handler.waitForTerminal(request.Context(), run.ID)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	handler.writeWaitResult(writer, request, run)
}

func (handler *Handler) decodeThreadRun(request *http.Request, writer http.ResponseWriter) (domain.ThreadID, RunRequest, error) {
	threadID, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		return "", RunRequest{}, err
	}
	if _, err = handler.ownedThread(request.Context(), threadID); err != nil {
		return "", RunRequest{}, err
	}
	var input RunRequest
	if err = decodeJSON(writer, request, &input); err != nil {
		return "", RunRequest{}, errResponseWritten
	}
	if err = validateRunRequest(input); err != nil {
		return "", RunRequest{}, err
	}
	return threadID, input, nil
}

func (handler *Handler) decodeStatelessRun(request *http.Request, writer http.ResponseWriter) (domain.ThreadID, RunRequest, error) {
	var input RunRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return "", RunRequest{}, errResponseWritten
	}
	if err := validateRunRequest(input); err != nil {
		return "", RunRequest{}, err
	}
	threadID, err := configuredThreadID(input.Config)
	if err != nil {
		return "", RunRequest{}, err
	}
	if threadID != "" {
		if _, err = handler.ownedThread(request.Context(), threadID); err != nil {
			return "", RunRequest{}, err
		}
		return threadID, input, nil
	}
	thread, err := domain.NewThread(handler.now())
	if err != nil {
		return "", RunRequest{}, err
	}
	thread.Metadata = map[string]string{store.OwnerMetadataKey: handler.ownerID(request.Context())}
	if input.AssistantID != "" {
		thread.Metadata["assistant_id"] = input.AssistantID
	}
	if err = handler.store.CreateThread(request.Context(), thread); err != nil {
		return "", RunRequest{}, err
	}
	return thread.ID, input, nil
}

func configuredThreadID(raw json.RawMessage) (domain.ThreadID, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}
	var config struct {
		Configurable struct {
			ThreadID string `json:"thread_id"`
		} `json:"configurable"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", errors.New("invalid config")
	}
	if strings.TrimSpace(config.Configurable.ThreadID) == "" {
		return "", nil
	}
	return domain.ParseThreadID(config.Configurable.ThreadID)
}

func (handler *Handler) streamStartedRun(writer http.ResponseWriter, request *http.Request, run domain.Run) {
	writer.Header().Set("Content-Location", "/api/threads/"+string(run.ThreadID)+"/runs/"+string(run.ID))
	request.SetPathValue("thread_id", string(run.ThreadID))
	request.SetPathValue("run_id", string(run.ID))
	handler.streamEvents(writer, request)
}

func (handler *Handler) waitForTerminal(ctx context.Context, runID domain.RunID) (domain.Run, error) {
	watch, err := handler.store.Watch(ctx, runID, 0)
	if err != nil {
		return domain.Run{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, lookupErr := handler.store.Run(ctx, runID)
		if lookupErr != nil || run.Terminal() {
			return run, lookupErr
		}
		select {
		case <-ctx.Done():
			return domain.Run{}, ctx.Err()
		case <-watch:
		case <-ticker.C:
		}
	}
}

func (handler *Handler) writeWaitResult(writer http.ResponseWriter, request *http.Request, run domain.Run) {
	messages, err := conversation.Load(request.Context(), handler.store, run.ThreadID)
	if err != nil {
		writeError(writer, err)
		return
	}
	result := map[string]any{"messages": messages}
	if run.Status != domain.RunSucceeded {
		result["status"] = makeRunResponse(run).Status
		if run.Error != "" {
			result["error"] = run.Error
		}
	}
	writer.Header().Set("Content-Location", "/api/threads/"+string(run.ThreadID)+"/runs/"+string(run.ID))
	writeJSON(writer, http.StatusOK, result)
}

func (handler *Handler) listMessagesByRunID(writer http.ResponseWriter, request *http.Request) {
	runID, err := domain.ParseRunID(request.PathValue("run_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	run, err := handler.store.Run(request.Context(), runID)
	if err == nil {
		_, err = handler.ownedThread(request.Context(), run.ThreadID)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	messages, err := conversation.LoadRun(request.Context(), handler.store, run.ID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": messages, "has_more": false})
}

func (handler *Handler) postStreamEvents(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	action := request.URL.Query().Get("action")
	if action != "" && action != "interrupt" && action != "rollback" {
		writeError(writer, errors.New("invalid action"))
		return
	}
	wait := request.URL.Query().Get("wait")
	if wait != "" && wait != "0" && wait != "1" {
		writeError(writer, errors.New("invalid wait"))
		return
	}
	if action != "" {
		if handler.canceller == nil {
			writeJSON(writer, http.StatusNotImplemented, map[string]string{"error": "run cancellation is not configured"})
			return
		}
		if err = handler.canceller.Cancel(request.Context(), run.ID); err != nil {
			writeError(writer, err)
			return
		}
		if wait == "1" {
			if _, err = handler.waitForTerminal(request.Context(), run.ID); err != nil {
				writeError(writer, err)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}
	handler.streamEvents(writer, request)
}

var errResponseWritten = errors.New("response already written")

func writeCompatError(writer http.ResponseWriter, err error) {
	if !errors.Is(err, errResponseWritten) {
		writeError(writer, err)
	}
}
