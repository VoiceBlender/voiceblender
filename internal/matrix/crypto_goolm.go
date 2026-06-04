//go:build goolm

// Megolm-based encryption support for matrix legs. Compiled in only when the
// `goolm` build tag is set (e.g. `go build -tags goolm`); without the tag,
// crypto_stub.go provides no-op replacements and every send goes plaintext.
//
// State (Olm sessions, megolm inbound/outbound sessions, device tracking,
// account secrets) lives in an ephemeral SQLite :memory: database backed by
// the pure-Go modernc.org/sqlite driver. Every process restart yields a new
// device id from the peers' perspective — fine for the calls-only use case
// where peers re-share keys on the next call.
package matrix

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	// Pure-Go SQLite driver. mautrix's "string path" overload uses the cgo
	// mattn/go-sqlite3 driver via "sqlite3-fk-wal"; we build *dbutil.Database
	// ourselves so CGO_ENABLED=0 builds keep working.
	_ "modernc.org/sqlite"

	// Register the pure-Go olm replacement before the mautrix/crypto init.
	// Without this the default libolm driver loads, which requires the
	// system libolm C library and CGO.
	_ "maunium.net/go/mautrix/crypto/goolm"
)

// cryptoHandle wraps a mautrix CryptoHelper plus the SQLite db that backs it.
// All methods are nil-safe — calling them on a nil handle is the "no
// encryption configured" path and silently falls back to plaintext.
type cryptoHandle struct {
	helper *cryptohelper.CryptoHelper
	db     *sql.DB
	log    *slog.Logger
}

// newCryptoHandle builds an in-memory cryptohelper bound to cli. Caller
// must call h.Init(ctx) after cli.Syncer is installed.
func newCryptoHandle(cli *mautrix.Client, log *slog.Logger) (*cryptoHandle, error) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite :memory:: %w", err)
	}
	// SQLite :memory: with shared cache still requires capping the pool
	// to one connection to avoid migration races between connections.
	db.SetMaxOpenConns(1)

	wrapped, err := dbutil.NewWithDB(db, "sqlite")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wrap dbutil: %w", err)
	}

	pickleKey := make([]byte, 32)
	if _, err := rand.Read(pickleKey); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("generate pickle key: %w", err)
	}

	helper, err := cryptohelper.NewCryptoHelper(cli, pickleKey, wrapped)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("new cryptohelper: %w", err)
	}
	helper.DecryptErrorCallback = func(evt *event.Event, err error) {
		log.Warn("matrix: failed to decrypt event",
			"room_id", evt.RoomID, "event_id", evt.ID, "sender", evt.Sender, "error", err)
	}
	return &cryptoHandle{helper: helper, db: db, log: log}, nil
}

// Init initialises the underlying mautrix CryptoHelper. Must be called once,
// after cli.Syncer has been installed. Safe to call on a nil receiver.
func (c *cryptoHandle) Init(ctx context.Context) error {
	if c == nil || c.helper == nil {
		return nil
	}
	if err := c.helper.Init(ctx); err != nil {
		return fmt.Errorf("crypto init: %w", err)
	}
	return nil
}

// Close tears down the cryptohelper and the in-memory store. Idempotent and
// nil-safe.
func (c *cryptoHandle) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	if c.helper != nil {
		if err := c.helper.Close(); err != nil {
			firstErr = err
		}
		c.helper = nil
	}
	if c.db != nil {
		if err := c.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.db = nil
	}
	return firstErr
}

// SendOrEncrypt sends content to roomID. If the room is encrypted and the
// cryptohelper is initialised, content is encrypted with megolm and sent as
// m.room.encrypted; otherwise it goes out as plaintext under eventType.
func (c *cryptoHandle) SendOrEncrypt(
	ctx context.Context,
	cli *mautrix.Client,
	roomID id.RoomID,
	eventType event.Type,
	content any,
) error {
	if c != nil && c.helper != nil && c.roomEncrypted(ctx, cli, roomID) {
		encrypted, err := c.helper.Encrypt(ctx, roomID, eventType, content)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", eventType.Type, err)
		}
		_, err = cli.SendMessageEvent(ctx, roomID, event.EventEncrypted, encrypted)
		return err
	}
	_, err := cli.SendMessageEvent(ctx, roomID, eventType, content)
	return err
}

// roomEncrypted queries the mautrix StateStore to see whether the room has
// m.room.encryption state.
func (c *cryptoHandle) roomEncrypted(ctx context.Context, cli *mautrix.Client, roomID id.RoomID) bool {
	if cli == nil || cli.StateStore == nil {
		return false
	}
	store, ok := cli.StateStore.(crypto.StateStore)
	if !ok {
		return false
	}
	encrypted, err := store.IsEncrypted(ctx, roomID)
	if err != nil {
		return false
	}
	return encrypted
}

// cryptoCompiledIn reports whether the matrix package was built with the
// goolm tag (always true in this file). Used by callers that want to log
// whether encryption support is available.
func cryptoCompiledIn() bool { return true }
