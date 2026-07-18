package recording

import (
	"fmt"
	"os"
	"path/filepath"
)

// recordingFileMode is the mode a published recording carries. Recordings are
// captured to a staging file, and os.CreateTemp opens those 0600 while
// os.Rename preserves the inode mode, so the mode has to be restored before the
// rename or every published recording would silently become owner-only.
const recordingFileMode = 0o644

// stagingPattern names the staging files recordings are captured to. They live
// in the destination directory alongside the recordings themselves, so the name
// is dot-prefixed and distinct enough to recognise as residue.
const stagingPattern = ".rec-*.tmp"

// stagedFile is a recording being captured to a staging file in the directory
// it will eventually be published to. Nothing exists at finalPath until
// publishFile renames the staging file there.
type stagedFile struct {
	f         *os.File
	tmpPath   string
	finalPath string
}

// createStagedFile opens a staging file for a recording destined for finalPath.
// The staging file is created in the same directory as finalPath so the
// publishing rename stays within one filesystem, which is what makes it atomic.
func createStagedFile(finalPath string) (*stagedFile, error) {
	f, err := os.CreateTemp(filepath.Dir(finalPath), stagingPattern)
	if err != nil {
		return nil, err
	}
	return &stagedFile{f: f, tmpPath: f.Name(), finalPath: finalPath}, nil
}

// publishFile makes a staged recording durable and moves it to its final name
// in one atomic step, so the final name never refers to a partial file.
//
// The caller must have finished writing — including any trailing header rewrite
// — before calling this, because the sync here is what puts those bytes on
// disk.
//
// A failure before the rename drops the staging file and leaves nothing behind
// at either name. The rename is the point of no return: if the closing
// directory sync fails after it has landed, the recording stays published at its
// final name and an error is still returned, because the bytes are readable but
// their directory entry is not known to have survived a crash.
func publishFile(f *os.File, tmpPath, finalPath string) error {
	if err := syncForPublish(f); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close recording: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("publish recording: %w", err)
	}
	// The rename itself is only durable once the directory entry has reached
	// the disk, so the recording is not fully published until this returns.
	return syncDir(filepath.Dir(finalPath))
}

// syncForPublish flushes f to durable storage and gives it the mode a published
// recording is expected to carry. Both must happen before the rename: the sync
// so the final name never refers to unflushed bytes, and the chmod so the file
// is never visible at its final name with the wrong mode.
func syncForPublish(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync recording: %w", err)
	}
	if err := f.Chmod(recordingFileMode); err != nil {
		return fmt.Errorf("chmod recording: %w", err)
	}
	return nil
}

// syncDir flushes a directory's own entries to disk, which is what makes a
// rename into it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open recording dir: %w", err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("sync recording dir: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close recording dir: %w", err)
	}
	return nil
}

// discardTemp drops a staged recording that must not be published, leaving
// nothing at either the staging or the final name.
func discardTemp(f *os.File, tmpPath string) {
	f.Close()
	os.Remove(tmpPath)
}
