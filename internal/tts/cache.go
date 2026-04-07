package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Cache is a disk-backed TTS result cache. Each entry is stored as a single
// file whose name is the SHA-256 of the cache key. File format:
//
//	<mime-type>\n<raw audio bytes>
//
// The cache is safe for concurrent use. Entries persist across restarts.
type Cache struct {
	dir           string
	includeAPIKey bool
	log           *slog.Logger
}

// NewCache creates a Cache that stores entries under dir, creating the
// directory if it does not exist. When includeAPIKey is true, the API key is
// part of the cache key — use this when different keys map to different
// accounts with distinct voice clones or quotas.
func NewCache(dir string, includeAPIKey bool, log *slog.Logger) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tts cache: create dir %s: %w", dir, err)
	}
	return &Cache{dir: dir, includeAPIKey: includeAPIKey, log: log}, nil
}

// WrapProvider returns a Provider that transparently caches synthesis results
// on disk. providerName is included in the cache key to prevent cross-provider
// collisions.
func (c *Cache) WrapProvider(p Provider, providerName string) Provider {
	return &cachedProvider{cache: c, inner: p, prefix: providerName}
}

// Len returns the number of cached entries on disk.
func (c *Cache) Len() int {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

type cachedProvider struct {
	cache  *Cache
	inner  Provider
	prefix string
}

func (cp *cachedProvider) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	key := cp.prefix + "\x00" + opts.Voice + "\x00" + opts.ModelID + "\x00" +
		opts.Language + "\x00" + opts.Prompt + "\x00" + text
	if cp.cache.includeAPIKey {
		key += "\x00" + opts.APIKey
	}
	h := sha256.Sum256([]byte(key))
	path := filepath.Join(cp.cache.dir, fmt.Sprintf("%x", h))

	// Cache hit — serve from disk.
	if mimeType, data, err := readCacheFile(path); err == nil {
		return &Result{
			Audio:    io.NopCloser(bytes.NewReader(data)),
			MimeType: mimeType,
		}, nil
	}

	// Cache miss — call the underlying provider.
	result, err := cp.inner.Synthesize(ctx, text, opts)
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(result.Audio)
	result.Audio.Close()
	if err != nil {
		return nil, err
	}

	// Write to disk atomically (best-effort; don't fail synthesis on write error).
	if err := writeCacheFile(path, result.MimeType, data); err != nil {
		cp.cache.log.Warn("tts cache write failed", "path", path, "error", err)
	}

	return &Result{
		Audio:    io.NopCloser(bytes.NewReader(data)),
		MimeType: result.MimeType,
	}, nil
}

// writeCacheFile atomically writes a cache entry using a temp file + rename.
func writeCacheFile(path, mimeType string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tts-cache-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := fmt.Fprintf(f, "%s\n", mimeType); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// readCacheFile reads a cache entry written by writeCacheFile.
func readCacheFile(path string) (mimeType string, data []byte, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	idx := bytes.IndexByte(content, '\n')
	if idx < 0 {
		return "", nil, fmt.Errorf("tts cache: malformed entry %s", path)
	}
	return string(content[:idx]), content[idx+1:], nil
}
