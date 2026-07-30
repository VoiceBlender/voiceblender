package recording

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const maxRecordingBasenameLen = 128

var ErrInvalidRecordingFilename = errors.New("invalid recording filename")

// SanitizeBasename validates and normalizes a caller-supplied recording name.
// Only a single path segment is allowed; ".wav" is appended when missing.
func SanitizeBasename(requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return "", ErrInvalidRecordingFilename
	}
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return "", ErrInvalidRecordingFilename
	}

	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", ErrInvalidRecordingFilename
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		return "", ErrInvalidRecordingFilename
	}
	for _, r := range stem {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", ErrInvalidRecordingFilename
	}

	if ext == "" || !strings.EqualFold(ext, ".wav") {
		name = stem + ".wav"
	} else {
		name = stem + ".wav"
	}
	if len(name) > maxRecordingBasenameLen {
		return "", ErrInvalidRecordingFilename
	}
	return name, nil
}

func defaultRecordingBasename() string {
	return fmt.Sprintf("%s_%s.wav", time.Now().Format("20060102_150405"), uuid.New().String()[:8])
}

func resolveRecordingBasename(requested string) (string, error) {
	if requested == "" {
		return defaultRecordingBasename(), nil
	}
	return SanitizeBasename(requested)
}
