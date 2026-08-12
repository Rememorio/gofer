package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/feedback"
)

const feedbackColumns = `id,run_id,thread_id,user_id,message_id,rating,comment,created_at`

type feedbackState struct{ database *SQL }

// Create stores one user rating while enforcing one record per run and user.
func (stateStore *feedbackState) Create(ctx context.Context, entry feedback.Entry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	_, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`INSERT INTO gofer_feedback(`+feedbackColumns+`) VALUES(?,?,?,?,?,?,?,?)`), feedbackValues(entry)...)
	if err != nil && uniqueViolation(err) {
		return feedback.ErrExists
	}
	return err
}

// Upsert creates or updates the current user's run-level feedback atomically.
func (stateStore *feedbackState) Upsert(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string, rating int, comment string, at time.Time) (feedback.Entry, error) {
	entry, err := feedback.NewEntry(threadID, runID, userID, rating, "", comment, at)
	if err != nil {
		return feedback.Entry{}, err
	}
	statement := `INSERT INTO gofer_feedback(` + feedbackColumns + `) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(thread_id,run_id,user_id) DO UPDATE SET message_id='',rating=excluded.rating,comment=excluded.comment,created_at=excluded.created_at RETURNING ` + feedbackColumns
	row := stateStore.database.db.QueryRowContext(ctx, stateStore.database.bind(statement), feedbackValues(entry)...)
	return scanFeedback(row)
}

// Get returns one record only within its user boundary.
func (stateStore *feedbackState) Get(ctx context.Context, id domain.FeedbackID, userID string) (feedback.Entry, error) {
	lookup := feedback.Lookup{ID: id, UserID: userID}
	if err := lookup.Validate(); err != nil {
		return feedback.Entry{}, err
	}
	row := stateStore.database.db.QueryRowContext(ctx, stateStore.database.bind(`SELECT `+feedbackColumns+` FROM gofer_feedback WHERE id=? AND user_id=?`), id, strings.TrimSpace(userID))
	return scanFeedback(row)
}

// ListRun returns oldest-first feedback for the selected user and run.
func (stateStore *feedbackState) ListRun(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string, limit int) ([]feedback.Entry, error) {
	query, err := (feedback.Query{RunScope: feedback.RunScope{ThreadID: threadID, RunID: runID}, UserID: userID, Limit: limit}).Normalize()
	if err != nil {
		return nil, err
	}
	rows, err := stateStore.database.db.QueryContext(ctx, stateStore.database.bind(`SELECT `+feedbackColumns+` FROM gofer_feedback WHERE thread_id=? AND run_id=? AND user_id=? ORDER BY created_at,id LIMIT ?`), threadID, runID, query.UserID, query.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	entries := make([]feedback.Entry, 0)
	for rows.Next() {
		entry, scanErr := scanFeedback(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Stats aggregates all ratings for one thread-scoped run.
func (stateStore *feedbackState) Stats(ctx context.Context, threadID domain.ThreadID, runID domain.RunID) (feedback.Stats, error) {
	if err := (feedback.RunScope{ThreadID: threadID, RunID: runID}).Validate(); err != nil {
		return feedback.Stats{}, err
	}
	stats := feedback.Stats{RunID: runID}
	row := stateStore.database.db.QueryRowContext(ctx, stateStore.database.bind(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN rating=1 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN rating=-1 THEN 1 ELSE 0 END),0) FROM gofer_feedback WHERE thread_id=? AND run_id=?`), threadID, runID)
	if err := row.Scan(&stats.Total, &stats.Positive, &stats.Negative); err != nil {
		return feedback.Stats{}, err
	}
	return stats, nil
}

// Delete removes one user-owned feedback record.
func (stateStore *feedbackState) Delete(ctx context.Context, id domain.FeedbackID, userID string) error {
	lookup := feedback.Lookup{ID: id, UserID: userID}
	if err := lookup.Validate(); err != nil {
		return err
	}
	result, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_feedback WHERE id=? AND user_id=?`), id, strings.TrimSpace(userID))
	return feedbackDeleteResult(result, err)
}

// DeleteRunUser removes the current user's feedback for a run.
func (stateStore *feedbackState) DeleteRunUser(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string) error {
	query, err := (feedback.Query{RunScope: feedback.RunScope{ThreadID: threadID, RunID: runID}, UserID: userID, Limit: 1}).Normalize()
	if err != nil {
		return err
	}
	result, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_feedback WHERE thread_id=? AND run_id=? AND user_id=?`), threadID, runID, query.UserID)
	return feedbackDeleteResult(result, err)
}

// DeleteThread removes feedback for a deleted thread; SQL foreign keys also
// enforce this cleanup when the thread row is deleted first.
func (stateStore *feedbackState) DeleteThread(ctx context.Context, threadID domain.ThreadID) error {
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return errors.Join(feedback.ErrInvalid, err)
	}
	_, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_feedback WHERE thread_id=?`), threadID)
	return err
}

func scanFeedback(row rowScanner) (feedback.Entry, error) {
	var entry feedback.Entry
	var created string
	if err := row.Scan(&entry.ID, &entry.RunID, &entry.ThreadID, &entry.UserID, &entry.MessageID, &entry.Rating, &entry.Comment, &created); errors.Is(err, sql.ErrNoRows) {
		return feedback.Entry{}, feedback.ErrNotFound
	} else if err != nil {
		return feedback.Entry{}, err
	}
	entry.CreatedAt = parseTime(created)
	return entry, entry.Validate()
}

func feedbackValues(entry feedback.Entry) []any {
	return []any{entry.ID, entry.RunID, entry.ThreadID, entry.UserID, entry.MessageID, entry.Rating, entry.Comment, formatTime(entry.CreatedAt)}
}

func feedbackDeleteResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return feedback.ErrNotFound
	}
	return nil
}

var _ feedback.Store = (*feedbackState)(nil)
