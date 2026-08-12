package sqlstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Rememorio/gofer/internal/skill"
)

func TestSQLiteSkillStatePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gofer.db")
	database := openSQLite(t, path)
	key := skill.Key{Category: skill.CategoryCustom, Name: "demo"}
	state := database.SkillState()
	if enabled, found, err := state.Get(context.Background(), key); err != nil || found || enabled {
		t.Fatalf("Get(empty) = %v, %v, %v", enabled, found, err)
	}
	if err := state.Set(context.Background(), key, false); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openSQLite(t, path)
	defer func() { _ = database.Close() }()
	if enabled, found, err := database.SkillState().Get(context.Background(), key); err != nil || !found || enabled {
		t.Fatalf("Get(persisted) = %v, %v, %v", enabled, found, err)
	}
	if err := database.SkillState().Set(context.Background(), key, true); err != nil {
		t.Fatal(err)
	}
	if enabled, found, err := database.SkillState().Get(context.Background(), key); err != nil || !found || !enabled {
		t.Fatalf("Get(updated) = %v, %v, %v", enabled, found, err)
	}
}
