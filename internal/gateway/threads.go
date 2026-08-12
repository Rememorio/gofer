package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

func (handler *Handler) threadRoutes() {
	handler.mux.HandleFunc("GET /api/threads", handler.listThreads)
	handler.mux.HandleFunc("POST /api/threads/search", handler.searchThreads)
	handler.mux.HandleFunc("PATCH /api/threads/{thread_id}", handler.patchThread)
	handler.mux.HandleFunc("DELETE /api/threads/{thread_id}", handler.deleteThread)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/state", handler.getThreadState)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs", handler.listRuns)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/messages", handler.listThreadMessages)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/messages", handler.listRunMessages)
}

func (handler *Handler) listThreads(writer http.ResponseWriter, request *http.Request) {
	limit, offset, err := pageRange(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	query := store.ThreadQuery{OwnerID: handler.ownerID(request.Context()), Text: request.URL.Query().Get("q"), Limit: limit, Offset: offset}
	threads, err := handler.store.Threads(request.Context(), query)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"threads": threadResponses(threads), "count": len(threads)})
}

func (handler *Handler) searchThreads(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Metadata map[string]string `json:"metadata"`
		Query    string            `json:"query"`
		Limit    int               `json:"limit"`
		Offset   int               `json:"offset"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	delete(input.Metadata, store.OwnerMetadataKey)
	threads, err := handler.store.Threads(request.Context(), store.ThreadQuery{
		OwnerID: handler.ownerID(request.Context()), Text: input.Query, Metadata: input.Metadata,
		Limit: input.Limit, Offset: input.Offset,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, threadResponses(threads))
}

func (handler *Handler) patchThread(writer http.ResponseWriter, request *http.Request) {
	id, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	if _, err = handler.ownedThread(request.Context(), id); err != nil {
		writeError(writer, err)
		return
	}
	var input struct {
		Title    *string           `json:"title"`
		Metadata map[string]string `json:"metadata"`
	}
	if err = decodeJSON(writer, request, &input); err != nil {
		return
	}
	delete(input.Metadata, store.OwnerMetadataKey)
	if len(input.Metadata) == 0 {
		input.Metadata = nil
	}
	thread, err := handler.store.PatchThread(request.Context(), id, store.ThreadPatch{Title: input.Title, Metadata: input.Metadata}, handler.now())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, makeThreadResponse(thread))
}

func (handler *Handler) deleteThread(writer http.ResponseWriter, request *http.Request) {
	id, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	ownerID := handler.ownerID(request.Context())
	if _, err = handler.ownedThread(request.Context(), id); err == nil && handler.cleaner != nil {
		err = handler.cleaner.PrepareThreadDelete(request.Context(), id)
	}
	if err == nil {
		err = handler.store.DeleteThread(request.Context(), id)
	}
	if err == nil && handler.cleaner != nil {
		err = handler.cleaner.CleanupThread(request.Context(), id, ownerID)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"success": true, "thread_id": id})
}

func (handler *Handler) getThreadState(writer http.ResponseWriter, request *http.Request) {
	thread, err := handler.requestThread(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	messages, err := conversation.Load(request.Context(), handler.store, thread.ID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"values": map[string]any{"messages": messages, "title": thread.Title}, "next": []string{},
		"config": map[string]any{"configurable": map[string]any{"thread_id": thread.ID}},
	})
}

func (handler *Handler) listRuns(writer http.ResponseWriter, request *http.Request) {
	thread, err := handler.requestThread(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	runs, err := handler.store.Runs(request.Context(), thread.ID)
	if err != nil {
		writeError(writer, err)
		return
	}
	responses := make([]runResponse, len(runs))
	for index, run := range runs {
		responses[index], err = handler.enrichedRunResponse(request.Context(), run)
		if err != nil {
			writeError(writer, err)
			return
		}
	}
	writeJSON(writer, http.StatusOK, responses)
}

func (handler *Handler) listThreadMessages(writer http.ResponseWriter, request *http.Request) {
	thread, err := handler.requestThread(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	messages, err := conversation.Load(request.Context(), handler.store, thread.ID)
	if err != nil {
		writeError(writer, err)
		return
	}
	limit, err := messageLimit(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	writeJSON(writer, http.StatusOK, messages)
}

func (handler *Handler) listRunMessages(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	messages, err := conversation.LoadRun(request.Context(), handler.store, run.ID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, messages)
}

func (handler *Handler) requestThread(request *http.Request) (domain.Thread, error) {
	id, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		return domain.Thread{}, err
	}
	return handler.ownedThread(request.Context(), id)
}

func (handler *Handler) ownedThread(ctx context.Context, id domain.ThreadID) (domain.Thread, error) {
	thread, err := handler.store.Thread(ctx, id)
	if err != nil {
		return domain.Thread{}, err
	}
	if !store.ThreadOwnedBy(thread, handler.ownerID(ctx)) {
		return domain.Thread{}, store.ErrNotFound
	}
	return thread, nil
}

func (handler *Handler) ownerID(ctx context.Context) string {
	ownerID := strings.TrimSpace(handler.owner(ctx))
	if ownerID == "" {
		return "local"
	}
	return ownerID
}

func threadResponses(threads []domain.Thread) []threadResponse {
	responses := make([]threadResponse, len(threads))
	for index, thread := range threads {
		responses[index] = makeThreadResponse(thread)
	}
	return responses
}

func pageRange(request *http.Request) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	if err == nil {
		if raw := request.URL.Query().Get("offset"); raw != "" {
			offset, err = strconv.Atoi(raw)
		}
	}
	if err != nil || limit < 1 || limit > 200 || offset < 0 {
		return 0, 0, store.ErrInvalidQuery
	}
	return limit, offset, nil
}

func messageLimit(request *http.Request) (int, error) {
	raw := request.URL.Query().Get("limit")
	if raw == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, store.ErrInvalidQuery
	}
	return limit, nil
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func publicMetadata(metadata map[string]string) map[string]string {
	cloned := cloneMetadata(metadata)
	delete(cloned, store.OwnerMetadataKey)
	if cloned == nil {
		return map[string]string{}
	}
	return cloned
}
