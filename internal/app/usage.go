package app

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/Rememorio/gofer/internal/contextwindow"
	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/usage"
)

var errInvalidUsageQuery = errors.New("invalid token usage query")

type contextUsageResponse struct {
	TokenCount       int      `json:"token_count"`
	MaxContextTokens *int     `json:"max_context_tokens"`
	Percentage       *float64 `json:"percentage"`
}

type threadUsageResponse struct {
	usage.ThreadSummary
	ContextUsage *contextUsageResponse `json:"context_usage,omitempty"`
}

func (service *Service) usageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/threads/{thread_id}/token-usage", service.threadTokenUsage)
}

func (service *Service) threadTokenUsage(writer http.ResponseWriter, request *http.Request) {
	thread, err := service.ownedThread(request)
	if err != nil {
		writeUsageError(writer, err)
		return
	}
	includeActive, err := usageIncludeActive(request)
	if err != nil {
		writeUsageError(writer, err)
		return
	}
	summary, err := usage.Aggregate(request.Context(), service.store, thread.ID, includeActive)
	if err != nil {
		writeUsageError(writer, err)
		return
	}
	messages, err := conversation.Load(request.Context(), service.store, thread.ID)
	if err != nil {
		writeUsageError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, threadUsageResponse{
		ThreadSummary: summary,
		ContextUsage:  contextUsage(contextwindow.Estimate(messages), service.config.Runtime.MaxContextTokens),
	})
}

func usageIncludeActive(request *http.Request) (bool, error) {
	value := request.URL.Query().Get("include_active")
	if value == "" {
		return false, nil
	}
	include, err := strconv.ParseBool(value)
	if err != nil {
		return false, errInvalidUsageQuery
	}
	return include, nil
}

func contextUsage(tokens, maximum int) *contextUsageResponse {
	response := &contextUsageResponse{TokenCount: max(0, tokens)}
	if maximum <= 0 {
		return response
	}
	percentage := math.Round(float64(response.TokenCount)/float64(maximum)*1000) / 10
	response.MaxContextTokens = &maximum
	response.Percentage = &percentage
	return response
}

func writeUsageError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errInvalidUsageQuery), errors.Is(err, domain.ErrInvalidID):
		status = http.StatusBadRequest
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
