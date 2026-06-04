//go:build !goolm

// Stubs for the encryption integration used when the matrix package is built
// without the `goolm` tag. Every cryptoHandle method is a no-op or plaintext
// fallback; m.call.* events into encrypted rooms will be sent as plaintext
// (which other clients reject), and inbound m.room.encrypted events stay
// unreadable. The runtime behaviour matches v1 of the matrix leg.
package matrix

import (
	"context"
	"log/slog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// cryptoHandle is the disabled stub. All methods are nil-safe.
type cryptoHandle struct{}

func newCryptoHandle(_ *mautrix.Client, _ *slog.Logger) (*cryptoHandle, error) {
	// nil handle + nil error == "encryption not compiled in"; callers store
	// the nil pointer and the nil-safe methods below take care of the rest.
	return nil, nil
}

func (c *cryptoHandle) Init(_ context.Context) error { return nil }
func (c *cryptoHandle) Close() error                 { return nil }

// SendOrEncrypt unconditionally sends plaintext — the encrypted-room
// detection in crypto_goolm.go is absent in this build.
func (c *cryptoHandle) SendOrEncrypt(
	ctx context.Context,
	cli *mautrix.Client,
	roomID id.RoomID,
	eventType event.Type,
	content any,
) error {
	_, err := cli.SendMessageEvent(ctx, roomID, eventType, content)
	return err
}

func cryptoCompiledIn() bool { return false }
