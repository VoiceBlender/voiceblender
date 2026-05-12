package wsmedia

import (
	"fmt"
	"net/http"

	"github.com/gobwas/ws"
)

// UpgradeServer turns an incoming HTTP request into a Transport on the
// server side of a WebSocket conversation. The returned http.Header is a
// copy of the request headers as observed before upgrade (callers commonly
// filter this to surface X-/P- custom headers).
//
// On upgrade failure UpgradeServer has already written an HTTP error
// response to w.
func UpgradeServer(w http.ResponseWriter, r *http.Request, cfg Config) (*Transport, http.Header, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	codec, err := CodecFromConfig(cfg)
	if err != nil {
		return nil, nil, err
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return nil, nil, fmt.Errorf("wsmedia: upgrade: %w", err)
	}

	reqHdr := r.Header.Clone()
	t := newTransport(cfg, conn, ws.StateServerSide, codec, reqHdr)
	return t, reqHdr, nil
}
