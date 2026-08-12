package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

var errInvalidBranch = errors.New("invalid conversation branch")

type branchInput struct {
	MessageID  string   `json:"message_id"`
	MessageIDs []string `json:"message_ids"`
	Title      string   `json:"title"`
}

type branchResponse struct {
	ThreadID            domain.ThreadID  `json:"thread_id"`
	ParentThreadID      domain.ThreadID  `json:"parent_thread_id"`
	ParentCheckpointID  string           `json:"parent_checkpoint_id"`
	BranchedFromMessage domain.MessageID `json:"branched_from_message_id"`
	WorkspaceCloneMode  string           `json:"workspace_clone_mode"`
	HistorySeedMode     string           `json:"history_seed_mode"`
}

func (service *Service) branchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/threads/{thread_id}/branches", service.createBranch)
}

func (service *Service) createBranch(writer http.ResponseWriter, request *http.Request) {
	source, err := service.mutableThread(request)
	if err != nil {
		writeBranchError(writer, err)
		return
	}
	var input branchInput
	if err = decodeBranchJSON(writer, request, &input); err != nil {
		writeBranchError(writer, err)
		return
	}
	history, err := conversation.Load(request.Context(), service.store, source.ID)
	if err != nil {
		writeBranchError(writer, err)
		return
	}
	seed, target, latest, err := selectBranchHistory(history, input)
	if err != nil {
		writeBranchError(writer, err)
		return
	}
	createdAt := time.Now().UTC()
	branch, err := newBranchThread(source, target.ID, input.Title, createdAt)
	if err == nil {
		err = service.store.CreateThread(request.Context(), branch)
	}
	if err != nil {
		writeBranchError(writer, err)
		return
	}
	cloneMode, err := service.cloneBranchWorkspace(source.ID, branch.ID, latest)
	if err == nil {
		_, err = conversation.Seed(request.Context(), service.store, branch.ID, seed, createdAt)
	}
	if err != nil {
		service.rollbackBranch(request.Context(), branch.ID)
		writeBranchError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, branchResponse{
		ThreadID: branch.ID, ParentThreadID: source.ID,
		ParentCheckpointID: "message:" + string(target.ID), BranchedFromMessage: target.ID,
		WorkspaceCloneMode: cloneMode, HistorySeedMode: "seeded",
	})
}

func selectBranchHistory(history []domain.Message, input branchInput) ([]domain.Message, domain.Message, bool, error) {
	if strings.TrimSpace(input.MessageID) == "" {
		return nil, domain.Message{}, false, fmt.Errorf("%w: message_id is required", errInvalidBranch)
	}
	targets := append(append([]string(nil), input.MessageIDs...), input.MessageID)
	selected := make(map[domain.MessageID]struct{}, len(targets))
	for _, raw := range targets {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		messageID, err := domain.ParseMessageID(raw)
		if err != nil {
			return nil, domain.Message{}, false, fmt.Errorf("%w: %w", errInvalidBranch, err)
		}
		selected[messageID] = struct{}{}
	}
	endIndex := -1
	found := 0
	var target domain.Message
	for candidate := range history {
		message := history[candidate]
		if _, ok := selected[message.ID]; !ok {
			continue
		}
		if message.Role != domain.RoleAssistant {
			return nil, domain.Message{}, false, store.ErrConflict
		}
		found++
		endIndex = candidate
		if string(message.ID) == input.MessageID {
			target = message
		}
	}
	if found != len(selected) || target.ID == "" {
		return nil, domain.Message{}, false, store.ErrConflict
	}
	latest := true
	for _, message := range history[endIndex+1:] {
		if message.Role == domain.RoleUser || message.Role == domain.RoleAssistant {
			latest = false
			break
		}
	}
	seed := append([]domain.Message(nil), history[:endIndex+1]...)
	return seed, target, latest, nil
}

func newBranchThread(source domain.Thread, target domain.MessageID, title string, at time.Time) (domain.Thread, error) {
	branch, err := domain.NewThread(at)
	if err != nil {
		return domain.Thread{}, err
	}
	branch.Title = strings.TrimSpace(title)
	if branch.Title == "" {
		branch.Title = strings.TrimSpace(source.Title)
		if branch.Title == "" {
			branch.Title = "Branch"
		} else {
			branch.Title += " (branch)"
		}
	}
	if len(branch.Title) > 256 {
		return domain.Thread{}, fmt.Errorf("%w: title is too long", errInvalidBranch)
	}
	branch.Metadata = make(map[string]string, len(source.Metadata)+4)
	for key, value := range source.Metadata {
		branch.Metadata[key] = value
	}
	branch.Metadata["branch"] = "true"
	branch.Metadata["branch_parent_thread_id"] = string(source.ID)
	branch.Metadata["branch_parent_message_id"] = string(target)
	branch.Metadata["branch_created_at"] = at.Format(time.RFC3339Nano)
	return branch, nil
}

func (service *Service) cloneBranchWorkspace(sourceID, targetID domain.ThreadID, latest bool) (string, error) {
	if !latest {
		return "skipped_historical_turn", nil
	}
	err := service.workspaces.Clone(sourceID, targetID)
	if errors.Is(err, fs.ErrNotExist) {
		return "not_found", nil
	}
	if err != nil {
		return "", err
	}
	return "current_thread_best_effort", nil
}

func (service *Service) rollbackBranch(ctx context.Context, threadID domain.ThreadID) {
	_ = service.workspaces.Remove(threadID)
	_ = service.store.DeleteThread(ctx, threadID)
}

func decodeBranchJSON(writer http.ResponseWriter, request *http.Request, output *branchInput) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("%w: %w", errInvalidBranch, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body must contain one JSON value", errInvalidBranch)
	}
	return nil
}

func writeBranchError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, errInvalidBranch), errors.Is(err, domain.ErrInvalidID):
		status = http.StatusUnprocessableEntity
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
