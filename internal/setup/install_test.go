package setup

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njoerd114/reminderrelay/internal/state"
)

// withTempHome points $HOME at a temp dir for the duration of the test, so
// state.DefaultDBPath() resolves under it instead of the real user's home.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestResetStateDB_RemovesDatabaseAndSidecarFiles(t *testing.T) {
	withTempHome(t)

	dbPath, err := state.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(dbPath+suffix, []byte("data"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", dbPath+suffix, err)
		}
	}

	if err := ResetStateDB(); err != nil {
		t.Fatalf("ResetStateDB: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); !os.IsNotExist(err) {
			t.Errorf("%s still exists after ResetStateDB (stat err = %v)", dbPath+suffix, err)
		}
	}
}

func TestResetStateDB_NoExistingDatabase_NoError(t *testing.T) {
	withTempHome(t)

	if err := ResetStateDB(); err != nil {
		t.Errorf("ResetStateDB with no existing database: %v", err)
	}
}

func TestResetStateDB_OnlyMainFileExists(t *testing.T) {
	withTempHome(t)

	dbPath, err := state.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("data"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	}
	// No -wal/-shm sidecars this time — ResetStateDB must tolerate their absence.

	if err := ResetStateDB(); err != nil {
		t.Fatalf("ResetStateDB: %v", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("%s still exists after ResetStateDB", dbPath)
	}
}

func TestWizard_OfferStateDBReset_NoExistingDatabase_SkipsPromptSilently(t *testing.T) {
	withTempHome(t)

	var out bytes.Buffer
	wiz := NewWizard(strings.NewReader(""), &out, slog.Default())

	if err := wiz.offerStateDBReset(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no prompt output when no database exists, got %q", out.String())
	}
}

func TestWizard_OfferStateDBReset_UserConfirms_RemovesDatabase(t *testing.T) {
	withTempHome(t)

	dbPath, err := state.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("data"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	}

	var out bytes.Buffer
	wiz := NewWizard(strings.NewReader("y\n"), &out, slog.Default())

	if err := wiz.offerStateDBReset(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Error("database should have been removed after confirming reset")
	}
	if !strings.Contains(out.String(), "reset") {
		t.Errorf("expected reset confirmation in output, got %q", out.String())
	}
}

func TestWizard_OfferStateDBReset_UserDeclines_KeepsDatabase(t *testing.T) {
	withTempHome(t)

	dbPath, err := state.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("data"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	}

	var out bytes.Buffer
	wiz := NewWizard(strings.NewReader("n\n"), &out, slog.Default())

	if err := wiz.offerStateDBReset(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database should still exist after declining reset: %v", err)
	}
}

func TestWizard_OfferStateDBReset_DefaultOnEmptyInput_KeepsDatabase(t *testing.T) {
	withTempHome(t)

	dbPath, err := state.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("data"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	}

	var out bytes.Buffer
	// Pressing Enter with no input should take the default (false — keep it),
	// since dropping sync history is destructive enough to require an
	// explicit yes.
	wiz := NewWizard(strings.NewReader("\n"), &out, slog.Default())

	if err := wiz.offerStateDBReset(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database should still exist after default (no) response: %v", err)
	}
}
