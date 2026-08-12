package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Rememorio/gofer/internal/memory"
)

const memoryColumns = `id,user_id,thread_id,agent_id,text,tags,category,confidence,source,created_at,updated_at,expires_at`

type memoryState struct{ database *SQL }

// Upsert creates or updates an entry without allowing scope changes.
func (stateStore *memoryState) Upsert(ctx context.Context, entry memory.Entry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	tags, err := json.Marshal(entry.Tags)
	if err != nil {
		return err
	}
	arguments := memoryValues(entry, string(tags))
	result, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`UPDATE gofer_memory_entries SET text=?,tags=?,category=?,confidence=?,source=?,created_at=?,updated_at=?,expires_at=? WHERE id=? AND user_id=? AND thread_id=? AND agent_id=?`), entry.Text, string(tags), entry.Category, entry.Confidence, entry.Source, formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt), formatTime(entry.ExpiresAt), entry.ID, entry.Scope.UserID, entry.Scope.ThreadID, entry.Scope.AgentID)
	if err != nil {
		return err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if affected == 1 {
		return nil
	}
	_, err = stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`INSERT INTO gofer_memory_entries(`+memoryColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), arguments...)
	if err == nil {
		return nil
	}
	if !uniqueViolation(err) {
		return err
	}
	result, updateErr := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`UPDATE gofer_memory_entries SET text=?,tags=?,category=?,confidence=?,source=?,created_at=?,updated_at=?,expires_at=? WHERE id=? AND user_id=? AND thread_id=? AND agent_id=?`), entry.Text, string(tags), entry.Category, entry.Confidence, entry.Source, formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt), formatTime(entry.ExpiresAt), entry.ID, entry.Scope.UserID, entry.Scope.ThreadID, entry.Scope.AgentID)
	if updateErr != nil {
		return updateErr
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if affected != 1 {
		return memory.ErrInvalid
	}
	return nil
}

// Get returns one entry only when its exact scope matches.
func (stateStore *memoryState) Get(ctx context.Context, scope memory.Scope, id string) (memory.Entry, error) {
	if err := scope.Validate(); err != nil {
		return memory.Entry{}, err
	}
	row := stateStore.database.db.QueryRowContext(ctx, stateStore.database.bind(`SELECT `+memoryColumns+` FROM gofer_memory_entries WHERE id=? AND user_id=? AND thread_id=? AND agent_id=?`), id, scope.UserID, scope.ThreadID, scope.AgentID)
	return scanMemory(row)
}

// Delete removes one exact-scope entry.
func (stateStore *memoryState) Delete(ctx context.Context, scope memory.Scope, id string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	result, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_memory_entries WHERE id=? AND user_id=? AND thread_id=? AND agent_id=?`), id, scope.UserID, scope.ThreadID, scope.AgentID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return memory.ErrNotFound
	}
	return nil
}

// Clear removes every entry in one exact scope.
func (stateStore *memoryState) Clear(ctx context.Context, scope memory.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	_, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_memory_entries WHERE user_id=? AND thread_id=? AND agent_id=?`), scope.UserID, scope.ThreadID, scope.AgentID)
	return err
}

// Replace atomically imports all entries for one exact scope.
func (stateStore *memoryState) Replace(ctx context.Context, scope memory.Scope, entries []memory.Entry) error {
	if err := memory.ValidateReplacement(scope, entries); err != nil {
		return err
	}
	tx, err := stateStore.database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_memory_entries WHERE user_id=? AND thread_id=? AND agent_id=?`), scope.UserID, scope.ThreadID, scope.AgentID); err != nil {
		return err
	}
	for _, entry := range entries {
		tags, marshalErr := json.Marshal(entry.Tags)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err = tx.ExecContext(ctx, stateStore.database.bind(`INSERT INTO gofer_memory_entries(`+memoryColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), memoryValues(entry, string(tags))...); err != nil {
			if uniqueViolation(err) {
				return memory.ErrInvalid
			}
			return err
		}
	}
	return tx.Commit()
}

// Search retrieves hierarchy-compatible rows and applies deterministic ranking.
func (stateStore *memoryState) Search(ctx context.Context, query memory.Query) ([]memory.Match, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	formattedNow := formatTime(query.Now)
	statement := `SELECT ` + memoryColumns + ` FROM gofer_memory_entries WHERE user_id=? AND (thread_id='' OR thread_id=?) AND (agent_id='' OR agent_id=?) AND (expires_at='' OR expires_at>?)`
	rows, err := stateStore.database.db.QueryContext(ctx, stateStore.database.bind(statement), query.Scope.UserID, query.Scope.ThreadID, query.Scope.AgentID, formattedNow)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	entries := make([]memory.Entry, 0)
	for rows.Next() {
		entry, scanErr := scanMemory(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return memory.Rank(query, entries)
}

func scanMemory(row rowScanner) (memory.Entry, error) {
	var entry memory.Entry
	var tags, created, updated, expires string
	err := row.Scan(&entry.ID, &entry.Scope.UserID, &entry.Scope.ThreadID, &entry.Scope.AgentID, &entry.Text, &tags, &entry.Category, &entry.Confidence, &entry.Source, &created, &updated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Entry{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.Entry{}, err
	}
	if err = json.Unmarshal([]byte(tags), &entry.Tags); err != nil {
		return memory.Entry{}, err
	}
	entry.CreatedAt, entry.UpdatedAt, entry.ExpiresAt = parseTime(created), parseTime(updated), parseTime(expires)
	return entry, entry.Validate()
}

func memoryValues(entry memory.Entry, tags string) []any {
	return []any{entry.ID, entry.Scope.UserID, entry.Scope.ThreadID, entry.Scope.AgentID, entry.Text, tags, entry.Category, entry.Confidence, entry.Source, formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt), formatTime(entry.ExpiresAt)}
}

func uniqueViolation(err error) bool {
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate")
}

var _ memory.Store = (*memoryState)(nil)
