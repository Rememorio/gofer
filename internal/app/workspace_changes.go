package app

import (
	"context"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
	"github.com/Rememorio/gofer/internal/workspacechange"
)

func (service *Service) workspaceFinishHook(thread *workspace.Thread, baseline *workspacechange.Snapshot, runID domain.RunID) runtime.FinishHook {
	return runtime.FinishFunc(func(ctx context.Context, writer runtime.EventWriter) error {
		result, err := workspacechange.CompareCurrent(thread, baseline, workspacechange.Limits{})
		if err != nil {
			service.logger.Warn("workspace change capture failed", "run_id", runID, "error", err)
			return nil
		}
		if !result.HasChanges() {
			return nil
		}
		if err = writer.Append(ctx, event.WorkspaceChanges, workspacechange.NewEventPayload(result)); err != nil {
			service.logger.Warn("workspace change event failed", "run_id", runID, "error", err)
		}
		return nil
	})
}
