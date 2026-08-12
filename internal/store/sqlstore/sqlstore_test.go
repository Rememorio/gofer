package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
)

func TestSQLitePersistsCompleteLifecycleAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gofer.db")
	database := openSQLite(t, path)
	thread, run := persistLifecycle(t, database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openSQLite(t, path)
	t.Cleanup(func() { _ = database.Close() })
	assertPersistedLifecycle(t, ctx, database, thread, run)
}

func persistLifecycle(t *testing.T, database *SQL) (domain.Thread, domain.Run) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(100, 123).UTC()
	thread, _ := domain.NewThread(now)
	thread.Title = "durable"
	thread.Metadata = map[string]string{"owner": "test"}
	if err := database.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	run, _ := domain.NewRun(thread.ID, now)
	if err := database.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	created, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, now, map[string]string{"source": "test"})
	message, _ := event.NewDraft(thread.ID, run.ID, event.MessageDelta, now.Add(time.Second), map[string]string{"text": "hello"})
	records, err := database.Append(ctx, run.ID, 0, created, message)
	if err != nil || len(records) != 2 || records[1].Sequence != 2 {
		t.Fatalf("Append()=%#v %v", records, err)
	}
	running, err := database.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), "")
	if err != nil || running.Attempt != 1 {
		t.Fatalf("running=%#v %v", running, err)
	}
	finished, err := database.TransitionRun(ctx, run.ID, domain.RunRunning, domain.RunSucceeded, now.Add(2*time.Second), "")
	if err != nil || !finished.Terminal() {
		t.Fatalf("finished=%#v %v", finished, err)
	}
	return thread, run
}

func assertPersistedLifecycle(t *testing.T, ctx context.Context, database *SQL, thread domain.Thread, run domain.Run) {
	t.Helper()
	gotThread, err := database.Thread(ctx, thread.ID)
	if err != nil || gotThread.Title != thread.Title || gotThread.Metadata["owner"] != "test" {
		t.Fatalf("Thread()=%#v %v", gotThread, err)
	}
	gotRun, err := database.Run(ctx, run.ID)
	if err != nil || gotRun.Status != domain.RunSucceeded {
		t.Fatalf("Run()=%#v %v", gotRun, err)
	}
	gotEvents, err := database.Events(ctx, run.ID, 0, 1)
	if err != nil || len(gotEvents) != 1 {
		t.Fatalf("Events(limit)=%#v %v", gotEvents, err)
	}
	gotEvents, err = database.Events(ctx, run.ID, 1, 0)
	if err != nil || len(gotEvents) != 1 || string(gotEvents[0].Data) != `{"text":"hello"}` {
		t.Fatalf("Events(after)=%#v %v", gotEvents, err)
	}
}

func TestSQLiteOptimisticConflictsAndValidation(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := database.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateThread(ctx, thread); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate thread=%v", err)
	}
	missingRun, _ := domain.NewRun(thread.ID, now)
	missingRun.ThreadID = newThreadID(t)
	if err := database.CreateRun(ctx, missingRun); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing parent=%v", err)
	}
	run, _ := domain.NewRun(thread.ID, now)
	if err := database.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateRun(ctx, run); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate run=%v", err)
	}
	if _, err := database.TransitionRun(ctx, run.ID, domain.RunRunning, domain.RunSucceeded, now, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("transition=%v", err)
	}
	draft, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, now, nil)
	if _, err := database.Append(ctx, run.ID, 1, draft); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("sequence=%v", err)
	}
	wrong, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, now, nil)
	wrong.ThreadID = newThreadID(t)
	if _, err := database.Append(ctx, run.ID, 0, wrong); err == nil {
		t.Fatal("scope mismatch succeeded")
	}
	if _, err := database.Thread(ctx, newThreadID(t)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing thread=%v", err)
	}
	if _, err := database.Run(ctx, newRunID(t)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run=%v", err)
	}
	if _, err := database.Events(ctx, newRunID(t), 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing events=%v", err)
	}
}

func TestSQLiteConcurrentAppendHasSingleWinner(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	thread, run := seed(t, database)
	draft, _ := event.NewDraft(thread.ID, run.ID, event.MessageDelta, time.Now(), map[string]string{"x": "y"})
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := database.Append(context.Background(), run.ID, 0, draft)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrConflict) {
			conflict++
		} else {
			t.Fatalf("Append()=%v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestSQLiteWatchSeesDurableWritesAndCancellation(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	thread, run := seed(t, database)
	ctx, cancel := context.WithCancel(context.Background())
	updates, err := database.Watch(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	draft, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, time.Now(), nil)
	if _, err = database.Append(context.Background(), run.ID, 0, draft); err != nil {
		t.Fatal(err)
	}
	select {
	case latest := <-updates:
		if latest != 1 {
			t.Fatalf("latest=%d", latest)
		}
	case <-time.After(time.Second):
		t.Fatal("watch timeout")
	}
	cancel()
	select {
	case _, open := <-updates:
		if open {
			t.Fatal("watch remains open")
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not close")
	}
}

func TestOpenValidationAndBinding(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{{}, {Driver: "bad", DSN: "x"}, {Driver: SQLite}, {Driver: SQLite, DSN: ":memory:", PollInterval: time.Millisecond}} {
		if _, err := Open(context.Background(), config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Open(%#v)=%v", config, err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(cancelled, Config{Driver: SQLite, DSN: ":memory:"}); err == nil {
		t.Fatal("Open cancelled succeeded")
	}
	var nilStore *SQL
	if err := nilStore.Close(); err != nil {
		t.Fatal(err)
	}
	database := &SQL{driver: Postgres}
	if got := database.bind("a=? AND b=?"); got != "a=$1 AND b=$2" {
		t.Fatalf("bind=%q", got)
	}
	database.driver = SQLite
	if got := database.bind("a=?"); got != "a=?" {
		t.Fatalf("sqlite bind=%q", got)
	}
	if !errors.Is(classifyConflict(errors.New("duplicate key")), store.ErrConflict) {
		t.Fatal("duplicate was not conflict")
	}
	if !errors.Is(classifyConflict(errors.New("could not serialize access")), store.ErrConflict) {
		t.Fatal("serialization was not conflict")
	}
	sentinel := errors.New("other")
	if !errors.Is(classifyConflict(sentinel), sentinel) || !errors.Is(classifyNotFound("x", sentinel), sentinel) {
		t.Fatal("classification changed unknown error")
	}
	if got := parseTime("invalid"); !got.IsZero() {
		t.Fatalf("parse invalid=%v", got)
	}
}

func TestSQLiteRejectsInvalidObjectsAndDuplicateEvents(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	if err := database.CreateThread(context.Background(), domain.Thread{}); !errors.Is(err, domain.ErrInvalidThread) {
		t.Fatalf("invalid thread=%v", err)
	}
	if err := database.CreateRun(context.Background(), domain.Run{}); !errors.Is(err, domain.ErrInvalidRun) {
		t.Fatalf("invalid run=%v", err)
	}
	thread, run := seed(t, database)
	draft, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, time.Now(), nil)
	if _, err := database.Append(context.Background(), run.ID, 0, draft); err != nil {
		t.Fatal(err)
	}
	second := draft
	second.Time = second.Time.Add(time.Second)
	if _, err := database.Append(context.Background(), run.ID, 1, second); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate event=%v", err)
	}
	if _, err := database.Append(context.Background(), newRunID(t), 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append missing=%v", err)
	}
	if _, err := database.Watch(context.Background(), newRunID(t), 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("watch missing=%v", err)
	}
}

func openSQLite(t *testing.T, path string) *SQL {
	t.Helper()
	database, err := Open(context.Background(), Config{Driver: SQLite, DSN: path, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func seed(t *testing.T, database *SQL) (domain.Thread, domain.Run) {
	t.Helper()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := database.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	run, _ := domain.NewRun(thread.ID, now)
	if err := database.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return thread, run
}

func newThreadID(t *testing.T) domain.ThreadID {
	t.Helper()
	thread, _ := domain.NewThread(time.Now())
	return thread.ID
}
func newRunID(t *testing.T) domain.RunID {
	t.Helper()
	run, _ := domain.NewRun(newThreadID(t), time.Now())
	return run.ID
}
