package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/scheduler"
	"github.com/Rememorio/gofer/internal/store"
)

func (service *Service) openScheduler() error {
	if durable, ok := service.store.(scheduler.Store); ok {
		service.scheduled = durable
	} else {
		service.scheduled = scheduler.NewMemoryStore()
	}
	hostname, _ := os.Hostname()
	owner := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	engine, err := scheduler.New(scheduler.Config{
		Store: service.scheduled, Executor: service, Owner: owner,
		LeaseDuration: time.Duration(service.config.Scheduler.LeaseDurationSeconds) * time.Second,
		BatchSize:     service.config.Scheduler.BatchSize,
		PollInterval:  time.Duration(service.config.Scheduler.PollIntervalSeconds) * time.Second,
		OnError:       func(err error) { service.logger.Error("scheduler iteration failed", "error", err) },
	})
	if err != nil {
		return err
	}
	service.scheduler = engine
	return nil
}

func (service *Service) startScheduler() {
	if !service.config.Scheduler.Enabled {
		return
	}
	service.background.Add(1)
	go func() {
		defer service.background.Done()
		if err := service.scheduler.Run(service.ctx); err != nil && !errors.Is(err, context.Canceled) {
			service.logger.Error("scheduler stopped", "error", err)
		}
	}()
}

// Execute launches one leased scheduled task as a normal durable run.
func (service *Service) Execute(ctx context.Context, task scheduler.Task) (scheduler.DispatchResult, error) {
	threadID, err := service.ensureScheduledThread(ctx, task)
	if err != nil {
		return scheduler.DispatchResult{}, err
	}
	run, err := domain.NewRun(threadID, time.Now())
	if err != nil {
		return scheduler.DispatchResult{}, err
	}
	if err = service.store.CreateRun(ctx, run); err != nil {
		return scheduler.DispatchResult{}, err
	}
	draft, err := event.NewDraft(threadID, run.ID, event.RunCreated, time.Now(), map[string]string{"scheduled_task_id": task.ID})
	if err != nil {
		return scheduler.DispatchResult{}, err
	}
	if _, err = service.store.Append(ctx, run.ID, 0, draft); err != nil {
		return scheduler.DispatchResult{}, err
	}
	input, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": task.Prompt}}})
	launchContext := auth.WithPrincipal(ctx, auth.Principal{ID: task.UserID, Permissions: []auth.Permission{auth.Admin}})
	err = service.Start(launchContext, gateway.StartRequest{
		RunID: run.ID, ThreadID: threadID,
		Request: gateway.RunRequest{AssistantID: service.config.Models[0].Name, Input: input, Metadata: map[string]any{"scheduled_task_id": task.ID}},
	})
	if err != nil {
		return scheduler.DispatchResult{}, err
	}
	return scheduler.DispatchResult{RunID: string(run.ID), ThreadID: string(threadID)}, nil
}

func (service *Service) ensureScheduledThread(ctx context.Context, task scheduler.Task) (domain.ThreadID, error) {
	if task.ThreadID != "" {
		id, err := domain.ParseThreadID(task.ThreadID)
		if err != nil {
			return "", err
		}
		if _, err = service.store.Thread(ctx, id); err != nil {
			return "", err
		}
		return id, nil
	}
	thread, err := domain.NewThread(time.Now())
	if err != nil {
		return "", err
	}
	thread.Title = task.Title
	thread.Metadata = map[string]string{"scheduled_task_id": task.ID, "user_id": task.UserID}
	if err = service.store.CreateThread(ctx, thread); err != nil {
		return "", err
	}
	return thread.ID, nil
}

func (service *Service) schedulerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/scheduled-tasks", service.listScheduledTasks)
	mux.HandleFunc("POST /api/scheduled-tasks", service.createScheduledTask)
	mux.HandleFunc("GET /api/scheduled-tasks/{task_id}", service.getScheduledTask)
	mux.HandleFunc("PATCH /api/scheduled-tasks/{task_id}", service.updateScheduledTask)
	mux.HandleFunc("DELETE /api/scheduled-tasks/{task_id}", service.deleteScheduledTask)
	mux.HandleFunc("POST /api/scheduled-tasks/{task_id}/pause", service.pauseScheduledTask)
	mux.HandleFunc("POST /api/scheduled-tasks/{task_id}/resume", service.resumeScheduledTask)
	mux.HandleFunc("POST /api/scheduled-tasks/{task_id}/trigger", service.triggerScheduledTask)
}

func (service *Service) listScheduledTasks(writer http.ResponseWriter, request *http.Request) {
	tasks, err := service.scheduled.List(request.Context(), requestUser(request.Context()))
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	if threadID := request.URL.Query().Get("thread_id"); threadID != "" {
		filtered := tasks[:0]
		for _, task := range tasks {
			if task.ThreadID == threadID {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	writeSchedulerJSON(writer, http.StatusOK, tasks)
}

func (service *Service) createScheduledTask(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Title        string                 `json:"title"`
		Prompt       string                 `json:"prompt"`
		ThreadID     string                 `json:"thread_id"`
		ScheduleType scheduler.ScheduleType `json:"schedule_type"`
		Schedule     string                 `json:"schedule"`
		Timezone     string                 `json:"timezone"`
	}
	if err := decodeSchedulerJSON(writer, request, &input); err != nil {
		return
	}
	if input.Timezone == "" {
		input.Timezone = "UTC"
	}
	if input.ThreadID != "" {
		threadID, parseErr := domain.ParseThreadID(input.ThreadID)
		if parseErr != nil {
			writeSchedulerError(writer, parseErr)
			return
		}
		if _, err := service.store.Thread(request.Context(), threadID); err != nil {
			writeSchedulerError(writer, err)
			return
		}
	}
	now := time.Now().UTC()
	next, err := scheduler.NextRun(input.ScheduleType, input.Schedule, input.Timezone, now)
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	id, err := newScheduledTaskID()
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	task := scheduler.Task{ID: id, UserID: requestUser(request.Context()), ThreadID: input.ThreadID, Title: strings.TrimSpace(input.Title), Prompt: strings.TrimSpace(input.Prompt), ScheduleType: input.ScheduleType, Schedule: strings.TrimSpace(input.Schedule), Timezone: input.Timezone, Status: scheduler.Enabled, NextRunAt: next, CreatedAt: now, UpdatedAt: now}
	if err = service.scheduled.Create(request.Context(), task); err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writeSchedulerJSON(writer, http.StatusCreated, task)
}

func (service *Service) getScheduledTask(writer http.ResponseWriter, request *http.Request) {
	task, err := service.ownedScheduledTask(request)
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writeSchedulerJSON(writer, http.StatusOK, task)
}

func (service *Service) updateScheduledTask(writer http.ResponseWriter, request *http.Request) {
	var update scheduler.Update
	if err := decodeSchedulerJSON(writer, request, &update); err != nil {
		return
	}
	task, err := service.scheduled.Update(request.Context(), request.PathValue("task_id"), requestUser(request.Context()), update, time.Now().UTC())
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writeSchedulerJSON(writer, http.StatusOK, task)
}

func (service *Service) deleteScheduledTask(writer http.ResponseWriter, request *http.Request) {
	err := service.scheduled.Delete(request.Context(), request.PathValue("task_id"), requestUser(request.Context()))
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (service *Service) pauseScheduledTask(writer http.ResponseWriter, request *http.Request) {
	service.setScheduledStatus(writer, request, scheduler.Paused)
}

func (service *Service) resumeScheduledTask(writer http.ResponseWriter, request *http.Request) {
	service.setScheduledStatus(writer, request, scheduler.Enabled)
}

func (service *Service) setScheduledStatus(writer http.ResponseWriter, request *http.Request, status scheduler.Status) {
	task, err := service.scheduled.SetStatus(request.Context(), request.PathValue("task_id"), requestUser(request.Context()), status, time.Now().UTC())
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writeSchedulerJSON(writer, http.StatusOK, task)
}

func (service *Service) triggerScheduledTask(writer http.ResponseWriter, request *http.Request) {
	task, err := service.scheduled.Trigger(request.Context(), request.PathValue("task_id"), requestUser(request.Context()), time.Now().UTC())
	if err == nil {
		err = service.scheduler.RunTask(request.Context(), task)
	}
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	task, err = service.scheduled.Get(request.Context(), task.ID)
	if err != nil {
		writeSchedulerError(writer, err)
		return
	}
	writeSchedulerJSON(writer, http.StatusOK, task)
}

func (service *Service) ownedScheduledTask(request *http.Request) (scheduler.Task, error) {
	task, err := service.scheduled.Get(request.Context(), request.PathValue("task_id"))
	if err != nil {
		return scheduler.Task{}, err
	}
	if task.UserID != requestUser(request.Context()) {
		return scheduler.Task{}, scheduler.ErrNotFound
	}
	return task, nil
}

func requestUser(ctx context.Context) string {
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		return principal.ID
	}
	return "local"
}

func newScheduledTaskID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(raw[:]), nil
}

func decodeSchedulerJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeSchedulerJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeSchedulerJSON(writer, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON value"})
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeSchedulerError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, scheduler.ErrNotFound), errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, scheduler.ErrConflict), errors.Is(err, store.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrInvalid), errors.Is(err, domain.ErrInvalidID):
		status = http.StatusBadRequest
	}
	writeSchedulerJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}

func writeSchedulerJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

var _ scheduler.Executor = (*Service)(nil)
