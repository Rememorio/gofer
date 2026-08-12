package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/store"
)

func (service *Service) assistantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/assistants/search", service.searchAssistants)
	mux.HandleFunc("GET /api/assistants/{assistant_id}", service.getAssistant)
	mux.HandleFunc("GET /api/assistants/{assistant_id}/graph", service.getAssistantGraph)
	mux.HandleFunc("GET /api/assistants/{assistant_id}/schemas", service.getAssistantSchemas)
}

type assistantResource struct {
	AssistantID string         `json:"assistant_id"`
	GraphID     string         `json:"graph_id"`
	Name        string         `json:"name"`
	Config      map[string]any `json:"config"`
	Metadata    map[string]any `json:"metadata"`
	Description string         `json:"description,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Version     int            `json:"version"`
}

func (service *Service) searchAssistants(writer http.ResponseWriter, request *http.Request) {
	input := struct {
		GraphID string `json:"graph_id"`
		Name    string `json:"name"`
		Limit   int    `json:"limit"`
		Offset  int    `json:"offset"`
	}{Limit: 10}
	if request.ContentLength != 0 {
		if err := decodeAssistantJSON(writer, request, &input); err != nil {
			return
		}
	}
	if input.Limit < 1 || input.Limit > 100 || input.Offset < 0 {
		writeResourceError(writer, store.ErrInvalidQuery)
		return
	}
	assistants := service.assistants()
	filtered := make([]assistantResource, 0, len(assistants))
	for _, assistant := range assistants {
		if input.GraphID != "" && assistant.GraphID != input.GraphID || input.Name != "" && !strings.Contains(strings.ToLower(assistant.Name), strings.ToLower(input.Name)) {
			continue
		}
		filtered = append(filtered, assistant)
	}
	if input.Offset >= len(filtered) {
		filtered = []assistantResource{}
	} else {
		filtered = filtered[input.Offset:min(len(filtered), input.Offset+input.Limit)]
	}
	writeResourceJSON(writer, http.StatusOK, filtered)
}

func (service *Service) getAssistant(writer http.ResponseWriter, request *http.Request) {
	assistant, ok := service.findAssistant(request.PathValue("assistant_id"))
	if !ok {
		writeResourceError(writer, store.ErrNotFound)
		return
	}
	writeResourceJSON(writer, http.StatusOK, assistant)
}

func (service *Service) getAssistantGraph(writer http.ResponseWriter, request *http.Request) {
	assistant, ok := service.findAssistant(request.PathValue("assistant_id"))
	if !ok {
		writeResourceError(writer, store.ErrNotFound)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{
		"graph_id": assistant.GraphID,
		"nodes":    []map[string]string{{"id": "agent", "name": "agent"}, {"id": "tools", "name": "tools"}},
		"edges":    []map[string]string{{"source": "agent", "target": "tools"}, {"source": "tools", "target": "agent"}},
	})
}

func (service *Service) getAssistantSchemas(writer http.ResponseWriter, request *http.Request) {
	assistant, ok := service.findAssistant(request.PathValue("assistant_id"))
	if !ok {
		writeResourceError(writer, store.ErrNotFound)
		return
	}
	messageSchema := map[string]any{"type": "object", "properties": map[string]any{"messages": map[string]any{"type": "array"}}, "required": []string{"messages"}}
	writeResourceJSON(writer, http.StatusOK, map[string]any{
		"graph_id": assistant.GraphID, "input_schema": messageSchema,
		"output_schema": messageSchema, "state_schema": messageSchema,
		"config_schema": map[string]any{"type": "object"},
	})
}

func (service *Service) assistants() []assistantResource {
	now := time.Now().UTC()
	assistants := []assistantResource{assistantFromModel("lead_agent", service.config.Models[0], now, true)}
	for _, model := range service.config.Models {
		if model.Name != "lead_agent" {
			assistants = append(assistants, assistantFromModel(model.Name, model, now, false))
		}
	}
	return assistants
}

func assistantFromModel(id string, model config.ModelConfig, now time.Time, system bool) assistantResource {
	createdBy := "config"
	description := "Gofer agent using " + model.Model
	if system {
		createdBy = "system"
		description = "Gofer lead agent"
	}
	return assistantResource{
		AssistantID: id, GraphID: "lead_agent", Name: id,
		Config:      map[string]any{"configurable": map[string]string{"model": model.Name}},
		Metadata:    map[string]any{"created_by": createdBy, "model": model.Name},
		Description: description, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
}

func (service *Service) findAssistant(id string) (assistantResource, bool) {
	for _, assistant := range service.assistants() {
		if assistant.AssistantID == id {
			return assistant, true
		}
	}
	return assistantResource{}, false
}

func decodeAssistantJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeResourceJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeResourceJSON(writer, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON value"})
		return errors.New("multiple JSON values")
	}
	return nil
}
