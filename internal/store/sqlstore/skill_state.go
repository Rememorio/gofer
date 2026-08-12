package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Rememorio/gofer/internal/skill"
)

type skillState struct{ database *SQL }

// SkillState returns the SQL-backed skill enablement adapter.
func (database *SQL) SkillState() skill.StateStore { return skillState{database: database} }

func (state skillState) Get(ctx context.Context, key skill.Key) (bool, bool, error) {
	row := state.database.db.QueryRowContext(ctx, state.database.bind(`SELECT enabled FROM gofer_skill_state WHERE category=? AND name=?`), key.Category, key.Name)
	var enabled bool
	err := row.Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return enabled, err == nil, err
}

func (state skillState) Set(ctx context.Context, key skill.Key, enabled bool) error {
	statement := `INSERT INTO gofer_skill_state(category,name,enabled,updated_at) VALUES(?,?,?,?) ON CONFLICT(category,name) DO UPDATE SET enabled=excluded.enabled,updated_at=excluded.updated_at`
	_, err := state.database.db.ExecContext(ctx, state.database.bind(statement), key.Category, key.Name, enabled, formatTime(time.Now().UTC()))
	return err
}

var _ skill.StateStore = skillState{}
