package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
)

type controlState struct{ database *SQL }

// Load returns durable control state or an empty optimistic snapshot.
func (stateStore *controlState) Load(ctx context.Context, threadID domain.ThreadID) (control.State, error) {
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return control.State{}, errors.Join(control.ErrInvalid, err)
	}
	row := stateStore.database.db.QueryRowContext(ctx, stateStore.database.bind(`SELECT version,goal,todos,updated_at FROM gofer_control_state WHERE thread_id=?`), threadID)
	var state control.State
	var goal, todos, updated string
	state.ThreadID = threadID
	if err := row.Scan(&state.Version, &goal, &todos, &updated); errors.Is(err, sql.ErrNoRows) {
		state.Todos = []control.Todo{}
		return state, nil
	} else if err != nil {
		return control.State{}, err
	}
	if err := json.Unmarshal([]byte(goal), &state.Goal); err != nil {
		return control.State{}, err
	}
	if err := json.Unmarshal([]byte(todos), &state.Todos); err != nil {
		return control.State{}, err
	}
	state.UpdatedAt = parseTime(updated)
	return state, state.Validate()
}

// CompareAndSwap stores a validated state only at the expected version.
func (stateStore *controlState) CompareAndSwap(ctx context.Context, next control.State, expected uint64) (control.State, error) {
	if err := next.Validate(); err != nil {
		return control.State{}, err
	}
	goal, err := json.Marshal(next.Goal)
	if err != nil {
		return control.State{}, err
	}
	todos, err := json.Marshal(next.Todos)
	if err != nil {
		return control.State{}, err
	}
	next.Version = expected + 1
	var result sql.Result
	if expected == 0 {
		result, err = stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`INSERT INTO gofer_control_state(thread_id,version,goal,todos,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(thread_id) DO NOTHING`), next.ThreadID, next.Version, string(goal), string(todos), formatTime(next.UpdatedAt))
	} else {
		result, err = stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`UPDATE gofer_control_state SET version=?,goal=?,todos=?,updated_at=? WHERE thread_id=? AND version=?`), next.Version, string(goal), string(todos), formatTime(next.UpdatedAt), next.ThreadID, expected)
	}
	if err != nil {
		return control.State{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return control.State{}, err
	}
	if affected != 1 {
		return control.State{}, control.ErrConflict
	}
	return next, nil
}

// Delete removes control state. Missing state is a no-op.
func (stateStore *controlState) Delete(ctx context.Context, threadID domain.ThreadID) error {
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return errors.Join(control.ErrInvalid, err)
	}
	_, err := stateStore.database.db.ExecContext(ctx, stateStore.database.bind(`DELETE FROM gofer_control_state WHERE thread_id=?`), threadID)
	return err
}

var _ control.Store = (*controlState)(nil)
