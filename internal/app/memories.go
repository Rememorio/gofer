package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/memory"
)

func (service *Service) memoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory", service.getMemory)
	mux.HandleFunc("POST /api/memory/reload", service.reloadMemory)
	mux.HandleFunc("DELETE /api/memory", service.clearMemory)
	mux.HandleFunc("POST /api/memory/facts", service.createMemoryFact)
	mux.HandleFunc("PATCH /api/memory/facts/{fact_id}", service.patchMemoryFact)
	mux.HandleFunc("DELETE /api/memory/facts/{fact_id}", service.deleteMemoryFact)
	mux.HandleFunc("GET /api/memory/export", service.getMemory)
	mux.HandleFunc("POST /api/memory/import", service.importMemory)
	mux.HandleFunc("GET /api/memory/config", service.getMemoryConfig)
	mux.HandleFunc("GET /api/memory/status", service.getMemoryStatus)
}

type memoryFact struct {
	ID                string         `json:"id"`
	Content           string         `json:"content"`
	Category          string         `json:"category"`
	CategoryExtension *string        `json:"categoryExtension,omitempty"`
	Topics            []string       `json:"topics,omitempty"`
	Confidence        float64        `json:"confidence"`
	CreatedAt         string         `json:"createdAt"`
	UpdatedAt         string         `json:"updatedAt,omitempty"`
	ExpiresAt         string         `json:"expiresAt,omitempty"`
	Source            string         `json:"source"`
	SourceError       *string        `json:"sourceError,omitempty"`
	SchemaVersion     *int           `json:"schemaVersion,omitempty"`
	Status            *string        `json:"status,omitempty"`
	Scope             map[string]any `json:"scope,omitempty"`
	Revision          *int           `json:"revision,omitempty"`
	ConsolidatedAt    *string        `json:"consolidatedAt,omitempty"`
	ConsolidatedFrom  []string       `json:"consolidatedFrom,omitempty"`
}

type memoryContextSection struct {
	Summary   string `json:"summary"`
	UpdatedAt string `json:"updatedAt"`
}

type memoryUserContext struct {
	WorkContext     memoryContextSection `json:"workContext"`
	PersonalContext memoryContextSection `json:"personalContext"`
	TopOfMind       memoryContextSection `json:"topOfMind"`
}

type memoryHistoryContext struct {
	RecentMonths       memoryContextSection `json:"recentMonths"`
	EarlierContext     memoryContextSection `json:"earlierContext"`
	LongTermBackground memoryContextSection `json:"longTermBackground"`
}

type memoryResponse struct {
	Version     string               `json:"version"`
	Revision    *int                 `json:"revision,omitempty"`
	LastUpdated string               `json:"lastUpdated"`
	User        memoryUserContext    `json:"user"`
	History     memoryHistoryContext `json:"history"`
	Facts       []memoryFact         `json:"facts"`
}

type memoryConfigResponse struct {
	Enabled                     bool           `json:"enabled"`
	Mode                        string         `json:"mode"`
	InjectionEnabled            bool           `json:"injection_enabled"`
	ShutdownFlushTimeoutSeconds float64        `json:"shutdown_flush_timeout_seconds"`
	ManagerClass                string         `json:"manager_class"`
	BackendConfig               map[string]any `json:"backend_config"`
}

func (service *Service) getMemory(writer http.ResponseWriter, request *http.Request) {
	data, err := service.readMemory(request, memoryQueryFromRequest(request))
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, data)
}

func (service *Service) reloadMemory(writer http.ResponseWriter, request *http.Request) {
	data, err := service.readMemory(request, memory.Query{Limit: 100})
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, data)
}

func (service *Service) clearMemory(writer http.ResponseWriter, request *http.Request) {
	if service.memories == nil {
		writeMemoryError(writer, memory.ErrNotFound)
		return
	}
	scope := memory.Scope{UserID: requestUser(request.Context())}
	if err := service.memories.Clear(request.Context(), scope); err != nil {
		writeMemoryError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, emptyMemoryResponse())
}

func (service *Service) createMemoryFact(writer http.ResponseWriter, request *http.Request) {
	if service.memories == nil {
		writeMemoryError(writer, memory.ErrNotFound)
		return
	}
	var input struct {
		Content    string   `json:"content"`
		Category   string   `json:"category"`
		Topics     []string `json:"topics"`
		Confidence *float64 `json:"confidence"`
		Source     string   `json:"source"`
		TTLSeconds int      `json:"ttl_seconds"`
	}
	if err := decodeMemoryJSON(writer, request, &input); err != nil {
		writeMemoryError(writer, err)
		return
	}
	if input.TTLSeconds < 0 || input.TTLSeconds > 31_536_000 {
		writeMemoryError(writer, memory.ErrInvalid)
		return
	}
	id, err := memory.NewID()
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	entry := newMemoryEntry(request, id, input.Content, input.Category, input.Topics, input.Confidence, input.Source, input.TTLSeconds)
	if err = service.memories.Upsert(request.Context(), entry); err != nil {
		writeMemoryError(writer, err)
		return
	}
	service.writeAllMemory(writer, request)
}

func (service *Service) patchMemoryFact(writer http.ResponseWriter, request *http.Request) {
	if service.memories == nil {
		writeMemoryError(writer, memory.ErrNotFound)
		return
	}
	scope := memory.Scope{UserID: requestUser(request.Context())}
	entry, err := service.memories.Get(request.Context(), scope, request.PathValue("fact_id"))
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	var input memoryFactPatch
	if err = decodeMemoryJSON(writer, request, &input); err == nil {
		err = applyMemoryFactPatch(&entry, input, time.Now().UTC())
	}
	if err == nil {
		err = service.memories.Upsert(request.Context(), entry)
	}
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	service.writeAllMemory(writer, request)
}

type memoryFactPatch struct {
	Content    *string    `json:"content"`
	Category   *string    `json:"category"`
	Topics     *[]string  `json:"topics"`
	Confidence *float64   `json:"confidence"`
	Source     *string    `json:"source"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func applyMemoryFactPatch(entry *memory.Entry, input memoryFactPatch, now time.Time) error {
	changed := false
	if input.Content != nil {
		entry.Text, changed = strings.TrimSpace(*input.Content), true
	}
	if input.Category != nil {
		entry.Category, changed = strings.TrimSpace(*input.Category), true
	}
	if input.Topics != nil {
		entry.Tags, changed = append([]string(nil), (*input.Topics)...), true
	}
	if input.Confidence != nil {
		entry.Confidence, changed = *input.Confidence, true
	}
	if input.Source != nil {
		entry.Source, changed = strings.TrimSpace(*input.Source), true
	}
	if input.ExpiresAt != nil {
		entry.ExpiresAt, changed = input.ExpiresAt.UTC(), true
	}
	if !changed {
		return memory.ErrInvalid
	}
	entry.UpdatedAt = now
	return entry.Validate()
}

func (service *Service) deleteMemoryFact(writer http.ResponseWriter, request *http.Request) {
	if service.memories == nil {
		writeMemoryError(writer, memory.ErrNotFound)
		return
	}
	scope := memory.Scope{UserID: requestUser(request.Context())}
	if err := service.memories.Delete(request.Context(), scope, request.PathValue("fact_id")); err != nil {
		writeMemoryError(writer, err)
		return
	}
	service.writeAllMemory(writer, request)
}

func (service *Service) importMemory(writer http.ResponseWriter, request *http.Request) {
	if service.memories == nil {
		writeMemoryError(writer, memory.ErrNotFound)
		return
	}
	var input memoryResponse
	if err := decodeMemoryJSON(writer, request, &input); err != nil {
		writeMemoryError(writer, err)
		return
	}
	scope := memory.Scope{UserID: requestUser(request.Context())}
	entries, err := importedMemoryEntries(scope, input.Facts, time.Now().UTC())
	if err == nil {
		err = service.memories.Replace(request.Context(), scope, entries)
	}
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	service.writeAllMemory(writer, request)
}

func (service *Service) getMemoryConfig(writer http.ResponseWriter, _ *http.Request) {
	writeResourceJSON(writer, http.StatusOK, service.memoryConfig())
}

func (service *Service) getMemoryStatus(writer http.ResponseWriter, request *http.Request) {
	data, err := service.readMemory(request, memory.Query{Limit: 100})
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"config": service.memoryConfig(), "data": data})
}

func (service *Service) memoryConfig() memoryConfigResponse {
	return memoryConfigResponse{
		Enabled: service.memories != nil, Mode: "middleware", InjectionEnabled: service.memories != nil,
		ShutdownFlushTimeoutSeconds: 0, ManagerClass: "gofer", BackendConfig: map[string]any{"driver": service.config.Storage.Driver},
	}
}

func (service *Service) writeAllMemory(writer http.ResponseWriter, request *http.Request) {
	data, err := service.readMemory(request, memory.Query{Limit: 100})
	if err != nil {
		writeMemoryError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, data)
}

func (service *Service) readMemory(request *http.Request, query memory.Query) (memoryResponse, error) {
	if service.memories == nil {
		return memoryResponse{}, memory.ErrNotFound
	}
	query.Scope = memory.Scope{UserID: requestUser(request.Context())}
	query.Now = time.Now().UTC()
	if query.Limit == 0 {
		query.Limit = 100
	}
	matches, err := service.memories.Search(request.Context(), query)
	if err != nil {
		return memoryResponse{}, err
	}
	response := emptyMemoryResponse()
	response.Facts = make([]memoryFact, len(matches))
	for index, match := range matches {
		response.Facts[index] = memoryFactFromEntry(match.Entry)
	}
	if len(matches) > 0 {
		response.LastUpdated = matches[0].Entry.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return response, nil
}

func memoryQueryFromRequest(request *http.Request) memory.Query {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	var tags []string
	if value := strings.TrimSpace(request.URL.Query().Get("tags")); value != "" {
		tags = strings.Split(value, ",")
	}
	return memory.Query{Text: request.URL.Query().Get("q"), Tags: tags, Limit: limit}
}

func emptyMemoryResponse() memoryResponse {
	return memoryResponse{Version: "1.0", Facts: []memoryFact{}}
}

func newMemoryEntry(request *http.Request, id, content, category string, topics []string, confidence *float64, source string, ttlSeconds int) memory.Entry {
	now := time.Now().UTC()
	if strings.TrimSpace(category) == "" {
		category = "context"
	}
	value := 0.5
	if confidence != nil {
		value = *confidence
	}
	entry := memory.Entry{ID: id, Scope: memory.Scope{UserID: requestUser(request.Context())}, Text: strings.TrimSpace(content), Tags: topics, Category: strings.TrimSpace(category), Confidence: value, Source: strings.TrimSpace(source), CreatedAt: now, UpdatedAt: now}
	if ttlSeconds > 0 {
		entry.ExpiresAt = now.Add(time.Duration(ttlSeconds) * time.Second)
	}
	return entry
}

func memoryFactFromEntry(entry memory.Entry) memoryFact {
	fact := memoryFact{ID: entry.ID, Content: entry.Text, Category: entry.Category, Topics: append([]string(nil), entry.Tags...), Confidence: entry.Confidence, CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339Nano), Source: entry.Source, Scope: map[string]any{"user_id": entry.Scope.UserID}}
	if entry.Scope.ThreadID != "" {
		fact.Scope["thread_id"] = entry.Scope.ThreadID
	}
	if entry.Scope.AgentID != "" {
		fact.Scope["agent_id"] = entry.Scope.AgentID
	}
	if !entry.ExpiresAt.IsZero() {
		fact.ExpiresAt = entry.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return fact
}

func importedMemoryEntries(scope memory.Scope, facts []memoryFact, now time.Time) ([]memory.Entry, error) {
	entries := make([]memory.Entry, len(facts))
	for index, fact := range facts {
		id := strings.TrimSpace(fact.ID)
		if id == "" {
			var err error
			id, err = memory.NewID()
			if err != nil {
				return nil, err
			}
		}
		created, err := optionalMemoryTime(fact.CreatedAt, now)
		if err != nil {
			return nil, err
		}
		updated, err := optionalMemoryTime(fact.UpdatedAt, created)
		if err != nil {
			return nil, err
		}
		expires, err := optionalMemoryTime(fact.ExpiresAt, time.Time{})
		if err != nil {
			return nil, err
		}
		entries[index] = memory.Entry{ID: id, Scope: scope, Text: strings.TrimSpace(fact.Content), Tags: append([]string(nil), fact.Topics...), Category: strings.TrimSpace(fact.Category), Confidence: fact.Confidence, Source: strings.TrimSpace(fact.Source), CreatedAt: created, UpdatedAt: updated, ExpiresAt: expires}
		if entries[index].Category == "" {
			entries[index].Category = "context"
		}
	}
	return entries, memory.ValidateReplacement(scope, entries)
}

func optionalMemoryTime(value string, fallback time.Time) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.Join(memory.ErrInvalid, err)
	}
	return parsed.UTC(), nil
}

func decodeMemoryJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.Join(memory.ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return memory.ErrInvalid
	}
	return nil
}

func writeMemoryError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, memory.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, memory.ErrInvalid):
		status = http.StatusUnprocessableEntity
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
