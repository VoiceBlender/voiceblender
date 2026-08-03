package recording

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReserveFinalPath_RejectsInFlightAndExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "call.wav")

	if err := reserveFinalPath(path); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := reserveFinalPath(path); !errors.Is(err, ErrRecordingFilenameExists) {
		t.Fatalf("second reserve err = %v, want ErrRecordingFilenameExists", err)
	}
	releaseFinalPath(path)

	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := reserveFinalPath(path); !errors.Is(err, ErrRecordingFilenameExists) {
		t.Fatalf("reserve existing err = %v, want ErrRecordingFilenameExists", err)
	}
}
