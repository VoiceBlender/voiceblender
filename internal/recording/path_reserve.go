package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrRecordingFilenameExists is returned when a recording's final path is
// already reserved by an in-progress capture or already present on disk.
var ErrRecordingFilenameExists = errors.New("recording file already exists")

var (
	reservedPathsMu sync.Mutex
	reservedPaths   = map[string]struct{}{}
)

// reserveFinalPath claims finalPath for an in-progress capture so a second
// recording with the same name fails at start rather than silently overwriting
// at publish time. The claim is released by releaseFinalPath once the capture
// has published or been discarded; a file left on disk continues to block
// later reserves via os.Stat.
func reserveFinalPath(finalPath string) error {
	finalPath = filepath.Clean(finalPath)

	reservedPathsMu.Lock()
	defer reservedPathsMu.Unlock()

	if _, ok := reservedPaths[finalPath]; ok {
		return ErrRecordingFilenameExists
	}
	switch _, err := os.Stat(finalPath); {
	case err == nil:
		return ErrRecordingFilenameExists
	case !os.IsNotExist(err):
		return fmt.Errorf("stat recording path: %w", err)
	}
	reservedPaths[finalPath] = struct{}{}
	return nil
}

func releaseFinalPath(finalPath string) {
	finalPath = filepath.Clean(finalPath)
	reservedPathsMu.Lock()
	delete(reservedPaths, finalPath)
	reservedPathsMu.Unlock()
}
