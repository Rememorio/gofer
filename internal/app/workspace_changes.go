package app

import (
	"context"

	"github.com/Rememorio/gofer/internal/delivery"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
	"github.com/Rememorio/gofer/internal/workspacechange"
)

func (service *Service) deliveryFinishHook(thread *workspace.Thread, baseline *workspacechange.Snapshot, tracker *delivery.Tracker, runID domain.RunID) runtime.FinishHook {
	return runtime.FinishFunc(func(ctx context.Context, writer runtime.EventWriter) error {
		var producedPaths []string
		if baseline != nil {
			result, changedOutputs, err := workspacechange.ReviewCurrent(thread, baseline, workspacechange.Limits{})
			if err != nil {
				service.logger.Warn("workspace change capture failed", "run_id", runID, "error", err)
			} else {
				producedPaths = changedOutputs
				service.recordWorkspaceReview(ctx, writer, runID, result)
			}
		}
		receipt := tracker.Receipt(producedPaths)
		persistErr := delivery.Persist(ctx, writer, receipt)
		if persistErr != nil {
			service.logger.Warn("delivery receipt failed", "run_id", runID, "error", persistErr)
		}
		return delivery.CompletionError(receipt, persistErr)
	})
}

func (service *Service) recordWorkspaceReview(ctx context.Context, writer runtime.EventWriter, runID domain.RunID, result workspacechange.Result) {
	if !result.HasChanges() {
		return
	}
	if err := writer.Append(ctx, event.WorkspaceChanges, workspacechange.NewEventPayload(result)); err != nil {
		service.logger.Warn("workspace change event failed", "run_id", runID, "error", err)
	}
}
