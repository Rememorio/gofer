package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

type threadRowScanner interface{ Scan(...any) error }

func scanThread(row threadRowScanner) (domain.Thread, error) {
	var thread domain.Thread
	var metadata, created, updated string
	if err := row.Scan(&thread.ID, &thread.Title, &metadata, &created, &updated); err != nil {
		return domain.Thread{}, classifyNotFound("thread", err)
	}
	if err := json.Unmarshal([]byte(metadata), &thread.Metadata); err != nil {
		return domain.Thread{}, err
	}
	thread.CreatedAt, thread.UpdatedAt = parseTime(created), parseTime(updated)
	return thread, thread.Validate()
}

// Threads returns an owner-scoped, newest-first conversation page.
func (database *SQL) Threads(ctx context.Context, query store.ThreadQuery) ([]domain.Thread, error) {
	query, err := query.Normalize()
	if err != nil {
		return nil, err
	}
	statement, arguments := database.threadQuery(query)
	rows, err := database.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	threads := make([]domain.Thread, 0)
	for rows.Next() {
		thread, scanErr := scanThread(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		threads = append(threads, thread)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return threads, nil
}

// PatchThread transactionally merges mutable conversation fields.
func (database *SQL) PatchThread(ctx context.Context, id domain.ThreadID, patch store.ThreadPatch, at time.Time) (domain.Thread, error) {
	if err := patch.Validate(); err != nil || at.IsZero() {
		return domain.Thread{}, store.ErrInvalidQuery
	}
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.Thread{}, err
	}
	defer func() { _ = tx.Rollback() }()
	thread, err := scanThread(tx.QueryRowContext(ctx, database.bind(`SELECT id,title,metadata,created_at,updated_at FROM gofer_threads WHERE id=?`), id))
	if err != nil {
		return domain.Thread{}, err
	}
	applySQLThreadPatch(&thread, patch, at)
	if err = thread.Validate(); err != nil {
		return domain.Thread{}, err
	}
	metadata, err := json.Marshal(thread.Metadata)
	if err != nil {
		return domain.Thread{}, err
	}
	if _, err = tx.ExecContext(ctx, database.bind(`UPDATE gofer_threads SET title=?,metadata=?,updated_at=? WHERE id=?`), thread.Title, string(metadata), formatTime(thread.UpdatedAt), id); err != nil {
		return domain.Thread{}, classifyConflict(err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Thread{}, classifyConflict(err)
	}
	return thread, nil
}

// SetThreadTitleIfEmpty atomically assigns a generated title without
// overwriting an existing or concurrently user-edited title.
func (database *SQL) SetThreadTitleIfEmpty(ctx context.Context, id domain.ThreadID, title string, at time.Time) (domain.Thread, bool, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 500 || at.IsZero() {
		return domain.Thread{}, false, store.ErrInvalidQuery
	}
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.Thread{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	thread, err := scanThread(tx.QueryRowContext(ctx, database.bind(`SELECT id,title,metadata,created_at,updated_at FROM gofer_threads WHERE id=?`), id))
	if err != nil || thread.Title != "" {
		return thread, false, err
	}
	thread.Title = title
	thread.UpdatedAt = at.UTC()
	if err = thread.Validate(); err != nil {
		return domain.Thread{}, false, err
	}
	result, err := tx.ExecContext(ctx, database.bind(`UPDATE gofer_threads SET title=?,updated_at=? WHERE id=? AND title=''`), title, formatTime(at), id)
	if err != nil {
		return domain.Thread{}, false, classifyConflict(err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return domain.Thread{}, false, errors.Join(store.ErrConflict, err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Thread{}, false, classifyConflict(err)
	}
	return thread, true, nil
}

// DeleteThread transactionally removes a conversation with no actively executing runs.
func (database *SQL) DeleteThread(ctx context.Context, id domain.ThreadID) error {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = scanThread(tx.QueryRowContext(ctx, database.bind(`SELECT id,title,metadata,created_at,updated_at FROM gofer_threads WHERE id=?`), id)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, database.bind(`SELECT status FROM gofer_runs WHERE thread_id=?`), id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status domain.RunStatus
		if err = rows.Scan(&status); err != nil {
			_ = rows.Close()
			return err
		}
		if status != domain.RunSucceeded && status != domain.RunFailed && status != domain.RunCancelled && status != domain.RunInterrupted {
			_ = rows.Close()
			return store.ErrConflict
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	statements := []string{
		`DELETE FROM gofer_events WHERE run_id IN (SELECT id FROM gofer_runs WHERE thread_id=?)`,
		`DELETE FROM gofer_runs WHERE thread_id=?`,
		`DELETE FROM gofer_scheduled_tasks WHERE thread_id=?`,
		`DELETE FROM gofer_threads WHERE id=?`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, database.bind(statement), id); err != nil {
			return err
		}
	}
	return classifyConflict(tx.Commit())
}

// Runs returns the oldest-first execution history for one conversation.
func (database *SQL) Runs(ctx context.Context, threadID domain.ThreadID) ([]domain.Run, error) {
	if _, err := database.Thread(ctx, threadID); err != nil {
		return nil, err
	}
	rows, err := database.db.QueryContext(ctx, database.bind(`SELECT id,thread_id,status,attempt,error,created_at,started_at,finished_at FROM gofer_runs WHERE thread_id=? ORDER BY created_at,id`), threadID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]domain.Run, 0)
	for rows.Next() {
		run, scanErr := scanRunRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (database *SQL) threadQuery(query store.ThreadQuery) (string, []any) {
	ownerExpression := `json_extract(metadata,'$.user_id')`
	metadataExpression := `EXISTS (SELECT 1 FROM json_each(metadata) WHERE key=? AND CAST(value AS TEXT)=?)`
	if database.driver == Postgres {
		ownerExpression = `metadata::jsonb ->> 'user_id'`
		metadataExpression = `(metadata::jsonb ->> ?)=?`
	}
	clauses := []string{"(" + ownerExpression + "=? OR (?='local' AND COALESCE(" + ownerExpression + ",'')=''))"}
	arguments := []any{query.OwnerID, query.OwnerID}
	if query.Text != "" {
		clauses = append(clauses, `LOWER(title) LIKE ? ESCAPE '\'`)
		arguments = append(arguments, "%"+escapeLike(strings.ToLower(query.Text))+"%")
	}
	keys := make([]string, 0, len(query.Metadata))
	for key := range query.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		clauses = append(clauses, metadataExpression)
		arguments = append(arguments, key, query.Metadata[key])
	}
	arguments = append(arguments, query.Limit, query.Offset)
	statement := `SELECT id,title,metadata,created_at,updated_at FROM gofer_threads WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?`
	return database.bind(statement), arguments
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func applySQLThreadPatch(thread *domain.Thread, patch store.ThreadPatch, at time.Time) {
	if patch.Title != nil {
		thread.Title = strings.TrimSpace(*patch.Title)
	}
	if thread.Metadata == nil && patch.Metadata != nil {
		thread.Metadata = make(map[string]string)
	}
	for key, value := range patch.Metadata {
		thread.Metadata[key] = value
	}
	thread.UpdatedAt = at.UTC()
}
