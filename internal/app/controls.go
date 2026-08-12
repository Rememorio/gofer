package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

func (service *Service) controlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/threads/{thread_id}/goal", service.getThreadGoal)
	mux.HandleFunc("PUT /api/threads/{thread_id}/goal", service.setThreadGoal)
	mux.HandleFunc("DELETE /api/threads/{thread_id}/goal", service.clearThreadGoal)
	mux.HandleFunc("GET /api/threads/{thread_id}/control", service.getThreadControl)
	mux.HandleFunc("PUT /api/threads/{thread_id}/todos", service.replaceThreadTodos)
}

func (service *Service) getThreadGoal(writer http.ResponseWriter, request *http.Request) {
	state, err := service.ownedControlState(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"goal": state.Goal})
}

func (service *Service) setThreadGoal(writer http.ResponseWriter, request *http.Request) {
	thread, err := service.mutableControlThread(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	var input struct {
		Objective   string `json:"objective"`
		TokenBudget int    `json:"token_budget"`
	}
	var state control.State
	if err = decodeControlJSON(writer, request, &input); err == nil {
		state, err = service.controls.SetGoal(request.Context(), thread.ID, input.Objective, input.TokenBudget)
	}
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"goal": state.Goal})
}

func (service *Service) clearThreadGoal(writer http.ResponseWriter, request *http.Request) {
	thread, err := service.mutableControlThread(request)
	if err == nil {
		_, err = service.controls.ClearGoal(request.Context(), thread.ID)
	}
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"goal": nil})
}

func (service *Service) getThreadControl(writer http.ResponseWriter, request *http.Request) {
	state, err := service.ownedControlState(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, state)
}

func (service *Service) replaceThreadTodos(writer http.ResponseWriter, request *http.Request) {
	thread, err := service.mutableControlThread(request)
	if err != nil {
		writeControlError(writer, err)
		return
	}
	var input struct {
		Todos []control.Todo `json:"todos"`
	}
	if err = decodeControlJSON(writer, request, &input); err == nil {
		var state control.State
		state, err = service.controls.ReplaceTodos(request.Context(), thread.ID, input.Todos)
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, state)
			return
		}
	}
	writeControlError(writer, err)
}

func (service *Service) ownedControlState(request *http.Request) (control.State, error) {
	thread, err := service.ownedThread(request)
	if err != nil {
		return control.State{}, err
	}
	return service.controls.Snapshot(request.Context(), thread.ID)
}

func (service *Service) mutableControlThread(request *http.Request) (domain.Thread, error) {
	thread, err := service.ownedThread(request)
	if err != nil {
		return domain.Thread{}, err
	}
	runs, err := service.store.Runs(request.Context(), thread.ID)
	if err != nil {
		return domain.Thread{}, err
	}
	for _, run := range runs {
		if !run.Terminal() {
			return domain.Thread{}, store.ErrConflict
		}
	}
	return thread, nil
}

func decodeControlJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.Join(control.ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return control.ErrInvalid
	}
	return nil
}

func writeControlError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, control.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, control.ErrInvalid):
		status = http.StatusUnprocessableEntity
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
