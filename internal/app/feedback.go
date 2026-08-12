package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/feedback"
	"github.com/Rememorio/gofer/internal/store"
)

type feedbackInput struct {
	Rating    int    `json:"rating"`
	Comment   string `json:"comment"`
	MessageID string `json:"message_id"`
}

func (service *Service) feedbackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/threads/{thread_id}/runs/{run_id}/feedback", service.createRunFeedback)
	mux.HandleFunc("PUT /api/threads/{thread_id}/runs/{run_id}/feedback", service.upsertRunFeedback)
	mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/feedback", service.listRunFeedback)
	mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/feedback/stats", service.runFeedbackStats)
	mux.HandleFunc("DELETE /api/threads/{thread_id}/runs/{run_id}/feedback", service.deleteRunFeedback)
	mux.HandleFunc("DELETE /api/threads/{thread_id}/runs/{run_id}/feedback/{feedback_id}", service.deleteFeedback)
	mux.HandleFunc("GET /api/runs/{run_id}/feedback", service.listFeedbackByRunID)
}

func (service *Service) createRunFeedback(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err != nil {
		writeFeedbackError(writer, err)
		return
	}
	var input feedbackInput
	if err = decodeFeedbackJSON(writer, request, &input); err == nil {
		var entry feedback.Entry
		entry, err = feedback.NewEntry(thread.ID, run.ID, requestUser(request.Context()), input.Rating, input.MessageID, input.Comment, time.Now().UTC())
		if err == nil {
			err = service.feedback.Create(request.Context(), entry)
		}
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, entry)
			return
		}
	}
	writeFeedbackError(writer, err)
}

func (service *Service) upsertRunFeedback(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err != nil {
		writeFeedbackError(writer, err)
		return
	}
	var input struct {
		Rating  int    `json:"rating"`
		Comment string `json:"comment"`
	}
	if err = decodeFeedbackJSON(writer, request, &input); err == nil {
		var entry feedback.Entry
		entry, err = service.feedback.Upsert(request.Context(), thread.ID, run.ID, requestUser(request.Context()), input.Rating, input.Comment, time.Now().UTC())
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, entry)
			return
		}
	}
	writeFeedbackError(writer, err)
}

func (service *Service) listRunFeedback(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err == nil {
		var entries []feedback.Entry
		entries, err = service.feedback.ListRun(request.Context(), thread.ID, run.ID, requestUser(request.Context()), 100)
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, entries)
			return
		}
	}
	writeFeedbackError(writer, err)
}

func (service *Service) listFeedbackByRunID(writer http.ResponseWriter, request *http.Request) {
	run, thread, err := service.ownedFeedbackRunByID(request)
	if err == nil {
		var entries []feedback.Entry
		entries, err = service.feedback.ListRun(request.Context(), thread.ID, run.ID, requestUser(request.Context()), 100)
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, entries)
			return
		}
	}
	writeFeedbackError(writer, err)
}

func (service *Service) runFeedbackStats(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err == nil {
		var stats feedback.Stats
		stats, err = service.feedback.Stats(request.Context(), thread.ID, run.ID)
		if err == nil {
			writeResourceJSON(writer, http.StatusOK, stats)
			return
		}
	}
	writeFeedbackError(writer, err)
}

func (service *Service) deleteRunFeedback(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err == nil {
		err = service.feedback.DeleteRunUser(request.Context(), thread.ID, run.ID, requestUser(request.Context()))
	}
	if err != nil {
		writeFeedbackError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]bool{"success": true})
}

func (service *Service) deleteFeedback(writer http.ResponseWriter, request *http.Request) {
	thread, run, err := service.feedbackRun(request)
	if err != nil {
		writeFeedbackError(writer, err)
		return
	}
	feedbackID, err := domain.ParseFeedbackID(request.PathValue("feedback_id"))
	if err == nil {
		var entry feedback.Entry
		entry, err = service.feedback.Get(request.Context(), feedbackID, requestUser(request.Context()))
		if err == nil && (entry.ThreadID != thread.ID || entry.RunID != run.ID) {
			err = feedback.ErrNotFound
		}
		if err == nil {
			err = service.feedback.Delete(request.Context(), feedbackID, requestUser(request.Context()))
		}
	}
	if err != nil {
		writeFeedbackError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]bool{"success": true})
}

func (service *Service) feedbackRun(request *http.Request) (domain.Thread, domain.Run, error) {
	thread, err := service.ownedThread(request)
	if err != nil {
		return domain.Thread{}, domain.Run{}, err
	}
	runID, err := domain.ParseRunID(request.PathValue("run_id"))
	if err != nil {
		return domain.Thread{}, domain.Run{}, err
	}
	run, err := service.store.Run(request.Context(), runID)
	if err != nil || run.ThreadID != thread.ID {
		if err == nil {
			err = store.ErrNotFound
		}
		return domain.Thread{}, domain.Run{}, err
	}
	return thread, run, nil
}

func (service *Service) ownedFeedbackRunByID(request *http.Request) (domain.Run, domain.Thread, error) {
	runID, err := domain.ParseRunID(request.PathValue("run_id"))
	if err != nil {
		return domain.Run{}, domain.Thread{}, err
	}
	run, err := service.store.Run(request.Context(), runID)
	if err != nil {
		return domain.Run{}, domain.Thread{}, err
	}
	thread, err := service.store.Thread(request.Context(), run.ThreadID)
	if err != nil || !store.ThreadOwnedBy(thread, requestUser(request.Context())) {
		return domain.Run{}, domain.Thread{}, store.ErrNotFound
	}
	return run, thread, nil
}

func decodeFeedbackJSON(writer http.ResponseWriter, request *http.Request, output any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.Join(feedback.ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body must contain one JSON value", feedback.ErrInvalid)
	}
	return nil
}

func writeFeedbackError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, feedback.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, feedback.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, feedback.ErrInvalid), errors.Is(err, domain.ErrInvalidID):
		status = http.StatusBadRequest
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
