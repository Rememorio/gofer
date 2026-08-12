package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/feedback"
	"github.com/Rememorio/gofer/internal/memory"
	"github.com/Rememorio/gofer/internal/store"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// ErrInvalidConfig identifies an unsupported driver, DSN, or polling policy.
var ErrInvalidConfig = errors.New("invalid SQL store configuration")

// Driver identifies a supported database/sql driver and placeholder dialect.
type Driver string

const (
	// SQLite selects the pure-Go embedded SQLite driver.
	SQLite Driver = "sqlite"
	// Postgres selects the pgx PostgreSQL database/sql driver.
	Postgres Driver = "postgres"
)

// Config controls connection ownership, migrations, and watch polling.
type Config struct {
	Driver       Driver
	DSN          string
	PollInterval time.Duration
}

// SQL is a transactional durable store backed by database/sql.
type SQL struct {
	db           *sql.DB
	driver       Driver
	pollInterval time.Duration
}

// Open creates a connection pool, verifies it, and applies idempotent schema migrations.
func Open(ctx context.Context, config Config) (*SQL, error) {
	if config.Driver != SQLite && config.Driver != Postgres {
		return nil, fmt.Errorf("%w: driver must be sqlite or postgres", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.DSN) == "" {
		return nil, fmt.Errorf("%w: DSN is required", ErrInvalidConfig)
	}
	if config.PollInterval == 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.PollInterval < 10*time.Millisecond {
		return nil, fmt.Errorf("%w: poll interval is too short", ErrInvalidConfig)
	}
	driverName := string(config.Driver)
	if config.Driver == Postgres {
		driverName = "pgx"
	}
	db, err := sql.Open(driverName, config.DSN)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", config.Driver, err)
	}
	if config.Driver == SQLite {
		db.SetMaxOpenConns(1)
	}
	database := &SQL{db: db, driver: config.Driver, pollInterval: config.PollInterval}
	if err = db.PingContext(ctx); err == nil {
		err = database.migrate(ctx)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return database, nil
}

// Close releases the owned database connection pool.
func (database *SQL) Close() error {
	if database == nil || database.db == nil {
		return nil
	}
	return database.db.Close()
}

func (database *SQL) migrate(ctx context.Context) error {
	if database.driver == SQLite {
		if _, err := database.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("enable SQLite foreign keys: %w", err)
		}
	}
	for _, statement := range schema {
		if _, err := database.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate SQL store: %w", err)
		}
	}
	return nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS gofer_threads (id TEXT PRIMARY KEY,title TEXT NOT NULL,metadata TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS gofer_runs (id TEXT PRIMARY KEY,thread_id TEXT NOT NULL REFERENCES gofer_threads(id),status TEXT NOT NULL,attempt BIGINT NOT NULL,error TEXT NOT NULL,created_at TEXT NOT NULL,started_at TEXT NOT NULL,finished_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gofer_runs_thread_idx ON gofer_runs(thread_id,created_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_events (run_id TEXT NOT NULL REFERENCES gofer_runs(id),sequence BIGINT NOT NULL,event_id TEXT NOT NULL UNIQUE,thread_id TEXT NOT NULL,type TEXT NOT NULL,timestamp TEXT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(run_id,sequence))`,
	`CREATE TABLE IF NOT EXISTS gofer_scheduled_tasks (id TEXT PRIMARY KEY,user_id TEXT NOT NULL,thread_id TEXT NOT NULL,title TEXT NOT NULL,prompt TEXT NOT NULL,schedule_type TEXT NOT NULL,schedule TEXT NOT NULL,timezone TEXT NOT NULL,status TEXT NOT NULL,next_run_at TEXT NOT NULL,last_run_at TEXT NOT NULL,last_run_id TEXT NOT NULL,last_error TEXT NOT NULL,lease_owner TEXT NOT NULL,lease_expires_at TEXT NOT NULL,run_count BIGINT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gofer_scheduled_tasks_due_idx ON gofer_scheduled_tasks(status,next_run_at,lease_expires_at)`,
	`CREATE INDEX IF NOT EXISTS gofer_scheduled_tasks_user_idx ON gofer_scheduled_tasks(user_id,created_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_skill_state (category TEXT NOT NULL,name TEXT NOT NULL,enabled BOOLEAN NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(category,name))`,
	`CREATE TABLE IF NOT EXISTS gofer_control_state (thread_id TEXT PRIMARY KEY REFERENCES gofer_threads(id) ON DELETE CASCADE,version BIGINT NOT NULL,goal TEXT NOT NULL,todos TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS gofer_memory_entries (id TEXT PRIMARY KEY,user_id TEXT NOT NULL,thread_id TEXT NOT NULL,agent_id TEXT NOT NULL,text TEXT NOT NULL,tags TEXT NOT NULL,category TEXT NOT NULL,confidence DOUBLE PRECISION NOT NULL,source TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,expires_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gofer_memory_scope_idx ON gofer_memory_entries(user_id,thread_id,agent_id,updated_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_feedback (id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES gofer_runs(id) ON DELETE CASCADE,thread_id TEXT NOT NULL REFERENCES gofer_threads(id) ON DELETE CASCADE,user_id TEXT NOT NULL,message_id TEXT NOT NULL,rating BIGINT NOT NULL,comment TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(thread_id,run_id,user_id))`,
	`CREATE INDEX IF NOT EXISTS gofer_feedback_run_idx ON gofer_feedback(thread_id,run_id,created_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_channel_bindings (id TEXT PRIMARY KEY,user_id TEXT NOT NULL,provider TEXT NOT NULL,workspace_id TEXT NOT NULL,workspace_name TEXT NOT NULL,external_user_id TEXT NOT NULL,external_user_name TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(provider,workspace_id,external_user_id))`,
	`CREATE INDEX IF NOT EXISTS gofer_channel_bindings_user_idx ON gofer_channel_bindings(user_id,updated_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_channel_connect_codes (code TEXT PRIMARY KEY,user_id TEXT NOT NULL,provider TEXT NOT NULL,slot BIGINT NOT NULL,created_at TEXT NOT NULL,expires_at BIGINT NOT NULL,UNIQUE(user_id,provider,slot))`,
	`CREATE INDEX IF NOT EXISTS gofer_channel_connect_codes_owner_idx ON gofer_channel_connect_codes(user_id,provider,expires_at)`,
	`CREATE TABLE IF NOT EXISTS gofer_channel_conversations (binding_id TEXT NOT NULL REFERENCES gofer_channel_bindings(id) ON DELETE CASCADE,provider TEXT NOT NULL,chat_id TEXT NOT NULL,topic_id TEXT NOT NULL,thread_id TEXT NOT NULL REFERENCES gofer_threads(id) ON DELETE CASCADE,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(binding_id,chat_id,topic_id))`,
	`CREATE INDEX IF NOT EXISTS gofer_channel_conversations_thread_idx ON gofer_channel_conversations(thread_id)`,
	`CREATE TABLE IF NOT EXISTS gofer_channel_deliveries (delivery_key TEXT PRIMARY KEY,expires_at BIGINT NOT NULL,complete BOOLEAN NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gofer_channel_deliveries_expiry_idx ON gofer_channel_deliveries(expires_at)`,
}

// ControlState exposes a durable goal and todo store backed by this database.
func (database *SQL) ControlState() control.Store { return &controlState{database: database} }

// MemoryState exposes durable scoped memory backed by this database.
func (database *SQL) MemoryState() memory.Store { return &memoryState{database: database} }

// FeedbackState exposes durable run feedback backed by this database.
func (database *SQL) FeedbackState() feedback.Store { return &feedbackState{database: database} }

// ChannelState exposes durable identity, conversation, and delivery state.
func (database *SQL) ChannelState() channel.State { return &channelState{database: database} }

// CreateThread persists a validated thread transactionally.
func (database *SQL) CreateThread(ctx context.Context, thread domain.Thread) error {
	if err := thread.Validate(); err != nil {
		return err
	}
	metadata, err := json.Marshal(thread.Metadata)
	if err != nil {
		return err
	}
	_, err = database.db.ExecContext(ctx, database.bind(`INSERT INTO gofer_threads(id,title,metadata,created_at,updated_at) VALUES(?,?,?,?,?)`), thread.ID, thread.Title, string(metadata), formatTime(thread.CreatedAt), formatTime(thread.UpdatedAt))
	if err != nil {
		return classifyInsert("thread", err)
	}
	return nil
}

// Thread returns one isolated durable thread.
func (database *SQL) Thread(ctx context.Context, id domain.ThreadID) (domain.Thread, error) {
	row := database.db.QueryRowContext(ctx, database.bind(`SELECT id,title,metadata,created_at,updated_at FROM gofer_threads WHERE id=?`), id)
	return scanThread(row)
}

// CreateRun persists a validated run after its parent thread exists.
func (database *SQL) CreateRun(ctx context.Context, run domain.Run) error {
	if err := run.Validate(); err != nil {
		return err
	}
	_, err := database.db.ExecContext(ctx, database.bind(`INSERT INTO gofer_runs(id,thread_id,status,attempt,error,created_at,started_at,finished_at) VALUES(?,?,?,?,?,?,?,?)`), run.ID, run.ThreadID, run.Status, run.Attempt, run.Error, formatTime(run.CreatedAt), formatTime(run.StartedAt), formatTime(run.FinishedAt))
	if err != nil {
		return classifyInsert("run", err)
	}
	return nil
}

// Run returns one durable run.
func (database *SQL) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	return database.scanRun(database.db.QueryRowContext(ctx, database.bind(`SELECT id,thread_id,status,attempt,error,created_at,started_at,finished_at FROM gofer_runs WHERE id=?`), id))
}

func (database *SQL) scanRun(row *sql.Row) (domain.Run, error) {
	var run domain.Run
	var created, started, finished string
	if err := row.Scan(&run.ID, &run.ThreadID, &run.Status, &run.Attempt, &run.Error, &created, &started, &finished); err != nil {
		return domain.Run{}, classifyNotFound("run", err)
	}
	run.CreatedAt, run.StartedAt, run.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
	return run, run.Validate()
}

// TransitionRun atomically performs an optimistic lifecycle transition.
func (database *SQL) TransitionRun(ctx context.Context, id domain.RunID, expected, next domain.RunStatus, at time.Time, failure string) (domain.Run, error) {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, database.bind(`SELECT id,thread_id,status,attempt,error,created_at,started_at,finished_at FROM gofer_runs WHERE id=?`), id)
	run, err := scanRunRow(row)
	if err != nil {
		return domain.Run{}, err
	}
	if run.Status != expected {
		return domain.Run{}, store.ErrConflict
	}
	advanced, err := run.Transition(next, at, failure)
	if err != nil {
		return domain.Run{}, err
	}
	result, err := tx.ExecContext(ctx, database.bind(`UPDATE gofer_runs SET status=?,attempt=?,error=?,started_at=?,finished_at=? WHERE id=? AND status=?`), advanced.Status, advanced.Attempt, advanced.Error, formatTime(advanced.StartedAt), formatTime(advanced.FinishedAt), id, expected)
	if err != nil {
		return domain.Run{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return domain.Run{}, store.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return domain.Run{}, err
	}
	return advanced, nil
}

func scanRunRow(row interface{ Scan(...any) error }) (domain.Run, error) {
	var run domain.Run
	var created, started, finished string
	if err := row.Scan(&run.ID, &run.ThreadID, &run.Status, &run.Attempt, &run.Error, &created, &started, &finished); err != nil {
		return domain.Run{}, classifyNotFound("run", err)
	}
	run.CreatedAt, run.StartedAt, run.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
	return run, run.Validate()
}

// Append atomically commits ordered event drafts after expectedSequence.
func (database *SQL) Append(ctx context.Context, runID domain.RunID, expectedSequence uint64, drafts ...event.Draft) ([]event.Event, error) {
	tx, err := database.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var threadID domain.ThreadID
	if err = tx.QueryRowContext(ctx, database.bind(`SELECT thread_id FROM gofer_runs WHERE id=?`), runID).Scan(&threadID); err != nil {
		return nil, classifyNotFound("run", err)
	}
	var current uint64
	if err = tx.QueryRowContext(ctx, database.bind(`SELECT COALESCE(MAX(sequence),0) FROM gofer_events WHERE run_id=?`), runID).Scan(&current); err != nil {
		return nil, err
	}
	if current != expectedSequence {
		return nil, store.ErrConflict
	}
	committed := make([]event.Event, len(drafts))
	for index, draft := range drafts {
		if draft.RunID != runID || draft.ThreadID != threadID {
			return nil, errors.New("event scope does not match run")
		}
		record, commitErr := draft.Commit(current + uint64(index) + 1)
		if commitErr != nil {
			return nil, commitErr
		}
		_, err = tx.ExecContext(ctx, database.bind(`INSERT INTO gofer_events(run_id,sequence,event_id,thread_id,type,timestamp,data) VALUES(?,?,?,?,?,?,?)`), record.RunID, record.Sequence, record.ID, record.ThreadID, record.Kind, formatTime(record.Time), string(record.Data))
		if err != nil {
			return nil, classifyConflict(err)
		}
		committed[index] = record
	}
	if err = tx.Commit(); err != nil {
		return nil, classifyConflict(err)
	}
	return committed, nil
}

// Events returns committed events strictly after sequence.
func (database *SQL) Events(ctx context.Context, runID domain.RunID, sequence uint64, limit int) ([]event.Event, error) {
	query := `SELECT sequence,event_id,thread_id,run_id,type,timestamp,data FROM gofer_events WHERE run_id=? AND sequence>? ORDER BY sequence`
	arguments := []any{runID, sequence}
	if limit > 0 {
		query += ` LIMIT ?`
		arguments = append(arguments, limit)
	}
	rows, err := database.db.QueryContext(ctx, database.bind(query), arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]event.Event, 0)
	for rows.Next() {
		var record event.Event
		var timestamp, data string
		if err = rows.Scan(&record.Sequence, &record.ID, &record.ThreadID, &record.RunID, &record.Kind, &timestamp, &data); err != nil {
			return nil, err
		}
		record.Time = parseTime(timestamp)
		record.Data = json.RawMessage(data)
		if err = record.Validate(); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		if _, err = database.Run(ctx, runID); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// Watch polls the durable journal, supporting writes from other processes.
func (database *SQL) Watch(ctx context.Context, runID domain.RunID, after uint64) (<-chan uint64, error) {
	if _, err := database.Run(ctx, runID); err != nil {
		return nil, err
	}
	updates := make(chan uint64, 1)
	go database.poll(ctx, runID, after, updates)
	return updates, nil
}

func (database *SQL) poll(ctx context.Context, runID domain.RunID, after uint64, updates chan<- uint64) {
	defer close(updates)
	ticker := time.NewTicker(database.pollInterval)
	defer ticker.Stop()
	for {
		latest, err := database.latestSequence(ctx, runID)
		if err != nil {
			return
		}
		if latest > after {
			select {
			case updates <- latest:
				after = latest
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (database *SQL) latestSequence(ctx context.Context, runID domain.RunID) (uint64, error) {
	var latest uint64
	err := database.db.QueryRowContext(ctx, database.bind(`SELECT COALESCE(MAX(sequence),0) FROM gofer_events WHERE run_id=?`), runID).Scan(&latest)
	return latest, err
}

func (database *SQL) bind(query string) string {
	if database.driver != Postgres {
		return query
	}
	var builder strings.Builder
	index := 1
	for _, character := range query {
		if character == '?' {
			fmt.Fprintf(&builder, "$%d", index)
			index++
		} else {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}
func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
func classifyNotFound(kind string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", kind, store.ErrNotFound)
	}
	return err
}
func classifyInsert(kind string, err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") {
		return fmt.Errorf("%s: %w", kind, store.ErrExists)
	}
	if strings.Contains(lower, "foreign key") {
		return fmt.Errorf("%s parent: %w", kind, store.ErrNotFound)
	}
	return err
}
func classifyConflict(err error) error {
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") || strings.Contains(lower, "serialize") {
		return store.ErrConflict
	}
	return err
}

var _ store.Store = (*SQL)(nil)
