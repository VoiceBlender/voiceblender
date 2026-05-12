package wsmedia

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/gobwas/ws"
)

// DialClient opens a WebSocket connection to url and returns a Transport
// configured for client-side framing. The supplied cfg.Headers are sent on
// the upgrade request. The returned http.Header is the server's response
// header.
func DialClient(ctx context.Context, url string, cfg Config) (*Transport, http.Header, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	codec, err := CodecFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}

	dialer := ws.Dialer{}
	if len(cfg.Headers) > 0 {
		dialer.Header = ws.HandshakeHeaderHTTP(cfg.Headers)
	}

	conn, _, hs, err := dialer.Dial(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("wsmedia: dial: %w", err)
	}

	peerHdr := http.Header{}
	if hs.Protocol != "" {
		peerHdr.Set("Sec-WebSocket-Protocol", hs.Protocol)
	}

	t := newTransport(cfg, conn, ws.StateClientSide, codec, peerHdr)
	return t, peerHdr, nil
}

// newTransport is the shared constructor used by DialClient and
// UpgradeServer to assemble a Transport ready for Start.
func newTransport(cfg Config, conn net.Conn, side ws.State, codec AudioCodec, peerHdr http.Header) *Transport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:     cfg,
		conn:    conn,
		side:    side,
		codec:   codec,
		log:     cfg.Log,
		peerHdr: peerHdr,
		audioIn: newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	t.SetOnText(nil)
	t.SetOnControl(nil)
	return t
}
