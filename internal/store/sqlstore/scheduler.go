package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/scheduler"
)

const scheduledTaskColumns = `id,user_id,thread_id,title,prompt,schedule_type,schedule,timezone,status,next_run_at,last_run_at,last_run_id,last_error,lease_owner,lease_expires_at,run_count,created_at,updated_at`

// Create stores one validated scheduled task.
func (database *SQL) Create(ctx context.Context, task scheduler.Task) error {
	if err := task.Validate(); err != nil {
		return err
	}
	_, err := database.db.ExecContext(ctx, database.bind(`INSERT INTO gofer_scheduled_tasks(`+scheduledTaskColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), scheduledTaskValues(task)...)
	if err != nil {
		return schedulerConflict(err)
	}
	return nil
}

// Get returns one scheduled task by identifier.
func (database *SQL) Get(ctx context.Context, id string) (scheduler.Task, error) {
	row := database.db.QueryRowContext(ctx, database.bind(`SELECT `+scheduledTaskColumns+` FROM gofer_scheduled_tasks WHERE id=?`), id)
	return scanScheduledTask(row)
}

// List returns stable scheduled tasks for one user.
func (database *SQL) List(ctx context.Context, userID string) ([]scheduler.Task, error) {
	rows, err := database.db.QueryContext(ctx, database.bind(`SELECT `+scheduledTaskColumns+` FROM gofer_scheduled_tasks WHERE user_id=? ORDER BY created_at,id`), userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanScheduledTasks(rows)
}

// Update changes mutable task fields when the owned task is not leased.
func (database *SQL) Update(ctx context.Context, id, userID string, update scheduler.Update, at time.Time) (scheduler.Task, error) {
	task, err := database.Get(ctx, id)
	if err != nil || task.UserID != userID {
		return scheduler.Task{}, schedulerNotFound(err)
	}
	if task.Status == scheduler.Running {
		return scheduler.Task{}, scheduler.ErrConflict
	}
	if scheduledUpdateEmpty(update) {
		return scheduler.Task{}, scheduler.ErrInvalid
	}
	previousUpdate := task.UpdatedAt
	applyScheduledUpdate(&task, update)
	if scheduledUpdateChangesSchedule(update) {
		next, nextErr := scheduler.NextRun(task.ScheduleType, task.Schedule, task.Timezone, at)
		if nextErr != nil {
			return scheduler.Task{}, nextErr
		}
		task.NextRunAt = next
		if task.Status != scheduler.Paused {
			task.Status = scheduler.Enabled
		}
	}
	task.UpdatedAt = at
	if err = task.Validate(); err != nil {
		return scheduler.Task{}, err
	}
	result, err := database.db.ExecContext(ctx, database.bind(`UPDATE gofer_scheduled_tasks SET title=?,prompt=?,schedule_type=?,schedule=?,timezone=?,status=?,next_run_at=?,updated_at=? WHERE id=? AND user_id=? AND status<>? AND updated_at=?`), task.Title, task.Prompt, task.ScheduleType, task.Schedule, task.Timezone, task.Status, formatTime(task.NextRunAt), formatTime(at), id, userID, scheduler.Running, formatTime(previousUpdate))
	if err != nil {
		return scheduler.Task{}, err
	}
	if err = requireAffected(result); err != nil {
		return scheduler.Task{}, err
	}
	return database.Get(ctx, id)
}

// Delete removes an owned task that is not currently leased.
func (database *SQL) Delete(ctx context.Context, id, userID string) error {
	result, err := database.db.ExecContext(ctx, database.bind(`DELETE FROM gofer_scheduled_tasks WHERE id=? AND user_id=? AND status<>?`), id, userID, scheduler.Running)
	if err != nil {
		return err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if affected == 1 {
		return nil
	}
	return database.schedulerMissingOrConflict(ctx, id, userID)
}

// Trigger makes an owned non-running task immediately due.
func (database *SQL) Trigger(ctx context.Context, id, userID string, at time.Time) (scheduler.Task, error) {
	result, err := database.db.ExecContext(ctx, database.bind(`UPDATE gofer_scheduled_tasks SET status=?,next_run_at=?,updated_at=? WHERE id=? AND user_id=? AND status<>?`), scheduler.Enabled, formatTime(at), formatTime(at), id, userID, scheduler.Running)
	if err != nil {
		return scheduler.Task{}, err
	}
	if err = requireAffected(result); err != nil {
		return scheduler.Task{}, database.schedulerMissingOrConflict(ctx, id, userID)
	}
	return database.Get(ctx, id)
}

// Due returns an ordered bounded batch eligible for leasing.
func (database *SQL) Due(ctx context.Context, now time.Time, limit int) ([]scheduler.Task, error) {
	if limit < 1 {
		return nil, scheduler.ErrInvalid
	}
	formatted := formatTime(now)
	query := `SELECT ` + scheduledTaskColumns + ` FROM gofer_scheduled_tasks WHERE (status=? OR (status=? AND lease_expires_at<=?)) AND next_run_at<>'' AND next_run_at<=? AND (lease_expires_at='' OR lease_expires_at<=?) ORDER BY next_run_at,id LIMIT ?`
	rows, err := database.db.QueryContext(ctx, database.bind(query), scheduler.Enabled, scheduler.Running, formatted, formatted, formatted, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanScheduledTasks(rows)
}

// Claim atomically leases a due task at its expected occurrence.
func (database *SQL) Claim(ctx context.Context, id string, expectedNext time.Time, owner string, now, leaseUntil time.Time) (scheduler.Task, error) {
	query := `UPDATE gofer_scheduled_tasks SET status=?,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND next_run_at=? AND (status=? OR (status=? AND lease_expires_at<=?)) AND (lease_expires_at='' OR lease_expires_at<=?)`
	result, err := database.db.ExecContext(ctx, database.bind(query), scheduler.Running, owner, formatTime(leaseUntil), formatTime(now), id, formatTime(expectedNext), scheduler.Enabled, scheduler.Running, formatTime(now), formatTime(now))
	if err != nil {
		return scheduler.Task{}, err
	}
	if err = requireAffected(result); err != nil {
		return scheduler.Task{}, scheduler.ErrConflict
	}
	return database.Get(ctx, id)
}

// Finish releases a matching lease and records the dispatch outcome.
func (database *SQL) Finish(ctx context.Context, id, owner string, at, next time.Time, dispatch scheduler.DispatchResult, dispatchErr error) (scheduler.Task, error) {
	task, err := database.Get(ctx, id)
	if err != nil {
		return scheduler.Task{}, err
	}
	if task.Status != scheduler.Running || task.LeaseOwner != owner {
		return scheduler.Task{}, scheduler.ErrConflict
	}
	finishScheduledTask(&task, at, next, dispatch, dispatchErr)
	query := `UPDATE gofer_scheduled_tasks SET thread_id=?,status=?,next_run_at=?,last_run_at=?,last_run_id=?,last_error=?,lease_owner='',lease_expires_at='',run_count=?,updated_at=? WHERE id=? AND status=? AND lease_owner=?`
	result, err := database.db.ExecContext(ctx, database.bind(query), task.ThreadID, task.Status, formatTime(task.NextRunAt), formatTime(task.LastRunAt), task.LastRunID, task.LastError, task.RunCount, formatTime(task.UpdatedAt), id, scheduler.Running, owner)
	if err != nil {
		return scheduler.Task{}, err
	}
	if err = requireAffected(result); err != nil {
		return scheduler.Task{}, scheduler.ErrConflict
	}
	return database.Get(ctx, id)
}

// SetStatus pauses or enables one owned non-running task.
func (database *SQL) SetStatus(ctx context.Context, id, userID string, status scheduler.Status, at time.Time) (scheduler.Task, error) {
	if status != scheduler.Enabled && status != scheduler.Paused {
		return scheduler.Task{}, scheduler.ErrInvalid
	}
	task, err := database.Get(ctx, id)
	if err != nil || task.UserID != userID {
		return scheduler.Task{}, schedulerNotFound(err)
	}
	if task.Status == scheduler.Running {
		return scheduler.Task{}, scheduler.ErrConflict
	}
	if status == scheduler.Enabled && task.NextRunAt.IsZero() {
		task.NextRunAt, err = scheduler.NextRun(task.ScheduleType, task.Schedule, task.Timezone, at)
		if err != nil {
			return scheduler.Task{}, err
		}
	}
	result, err := database.db.ExecContext(ctx, database.bind(`UPDATE gofer_scheduled_tasks SET status=?,next_run_at=?,updated_at=? WHERE id=? AND user_id=? AND status<>?`), status, formatTime(task.NextRunAt), formatTime(at), id, userID, scheduler.Running)
	if err != nil {
		return scheduler.Task{}, err
	}
	if err = requireAffected(result); err != nil {
		return scheduler.Task{}, database.schedulerMissingOrConflict(ctx, id, userID)
	}
	return database.Get(ctx, id)
}

func scheduledTaskValues(task scheduler.Task) []any {
	return []any{task.ID, task.UserID, task.ThreadID, task.Title, task.Prompt, task.ScheduleType, task.Schedule, task.Timezone, task.Status, formatTime(task.NextRunAt), formatTime(task.LastRunAt), task.LastRunID, task.LastError, task.LeaseOwner, formatTime(task.LeaseExpiresAt), task.RunCount, formatTime(task.CreatedAt), formatTime(task.UpdatedAt)}
}

type rowScanner interface{ Scan(...any) error }

func scanScheduledTask(row rowScanner) (scheduler.Task, error) {
	var task scheduler.Task
	var next, last, lease, created, updated string
	err := row.Scan(&task.ID, &task.UserID, &task.ThreadID, &task.Title, &task.Prompt, &task.ScheduleType, &task.Schedule, &task.Timezone, &task.Status, &next, &last, &task.LastRunID, &task.LastError, &task.LeaseOwner, &lease, &task.RunCount, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return scheduler.Task{}, scheduler.ErrNotFound
	}
	if err != nil {
		return scheduler.Task{}, err
	}
	task.NextRunAt, task.LastRunAt, task.LeaseExpiresAt = parseTime(next), parseTime(last), parseTime(lease)
	task.CreatedAt, task.UpdatedAt = parseTime(created), parseTime(updated)
	return task, task.Validate()
}

func scanScheduledTasks(rows *sql.Rows) ([]scheduler.Task, error) {
	tasks := make([]scheduler.Task, 0)
	for rows.Next() {
		task, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func applyScheduledUpdate(task *scheduler.Task, update scheduler.Update) {
	if update.Title != nil {
		task.Title = strings.TrimSpace(*update.Title)
	}
	if update.Prompt != nil {
		task.Prompt = strings.TrimSpace(*update.Prompt)
	}
	if update.ScheduleType != nil {
		task.ScheduleType = *update.ScheduleType
	}
	if update.Schedule != nil {
		task.Schedule = strings.TrimSpace(*update.Schedule)
	}
	if update.Timezone != nil {
		task.Timezone = strings.TrimSpace(*update.Timezone)
	}
}

func scheduledUpdateEmpty(update scheduler.Update) bool {
	return update.Title == nil && update.Prompt == nil && update.ScheduleType == nil && update.Schedule == nil && update.Timezone == nil
}

func scheduledUpdateChangesSchedule(update scheduler.Update) bool {
	return update.ScheduleType != nil || update.Schedule != nil || update.Timezone != nil
}

func finishScheduledTask(task *scheduler.Task, at, next time.Time, result scheduler.DispatchResult, dispatchErr error) {
	task.LastRunAt, task.LastRunID, task.UpdatedAt = at, result.RunID, at
	if task.ThreadID == "" && result.ThreadID != "" {
		task.ThreadID = result.ThreadID
	}
	task.RunCount++
	task.LeaseOwner, task.LeaseExpiresAt = "", time.Time{}
	if dispatchErr != nil {
		task.LastError = dispatchErr.Error()
	} else {
		task.LastError = ""
	}
	if task.ScheduleType == scheduler.Once {
		task.NextRunAt = time.Time{}
		if dispatchErr != nil {
			task.Status = scheduler.Failed
		} else {
			task.Status = scheduler.Completed
		}
	} else {
		task.Status, task.NextRunAt = scheduler.Enabled, next
	}
}

func requireAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return scheduler.ErrConflict
	}
	return nil
}

func (database *SQL) schedulerMissingOrConflict(ctx context.Context, id, userID string) error {
	task, err := database.Get(ctx, id)
	if errors.Is(err, scheduler.ErrNotFound) || err == nil && task.UserID != userID {
		return scheduler.ErrNotFound
	}
	if err != nil {
		return err
	}
	return scheduler.ErrConflict
}

func schedulerConflict(err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") {
		return scheduler.ErrConflict
	}
	return err
}

func schedulerNotFound(err error) error {
	if err == nil || errors.Is(err, scheduler.ErrNotFound) {
		return scheduler.ErrNotFound
	}
	return err
}

var _ scheduler.Store = (*SQL)(nil)
