package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// splitHostPort parses an "ip:port" socket string. Empty input or parse
// failure returns ("", 0).
func splitHostPort(socket string) (string, int) {
	if socket == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(socket)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// errBranchLost is the cancel cause used to tear down losing fork branches.
// Plain (non-WaitAnswerForceCancelErr) so sipgo's WaitAnswer sends a real
// CANCEL on the wire instead of exiting silently.
var errBranchLost = errors.New("fork: another branch already won")

// inviteFork sends the same INVITE to every entry in opts.ForkTargets in
// parallel and returns the OutboundCall for the first branch to reach a
// 2xx response. Losing branches are CANCELled. Branches that race past a
// 200 OK (sipgo received the success before the CANCEL was processed) are
// ACK'd then BYE'd so no dialog is leaked at the peer.
//
// RTT, session timer negotiation, and digest-auth retries are out of scope
// for v1 forked INVITEs — callers that need those should single-target.
func (e *Engine) inviteFork(ctx context.Context, recipient sip.Uri, opts InviteOptions) (*OutboundCall, error) {
	rtpSess, err := NewRTPSessionFromAllocator(e.portAlloc)
	if err != nil {
		return nil, fmt.Errorf("create RTP session: %w", err)
	}

	codecs := e.codecs
	if len(opts.Codecs) > 0 {
		codecs = opts.Codecs
	}
	localIP := e.advertisedIPForRecipient(ctx, recipient.Host)
	sdpOffer := GenerateOffer(SDPConfig{
		LocalIP:           localIP,
		RTPPort:           rtpSess.LocalPort(),
		Codecs:            codecs,
		AMRWBOctetAligned: e.amrwbOctetAligned,
	})

	e.log.Info("outbound INVITE (forked)", "recipient", recipient.String(),
		"branches", len(opts.ForkTargets), "codecs", fmt.Sprintf("%v", codecs))

	// Per branch we rewrite the Request-URI to the binding's socket. The
	// AOR identity is preserved on the To header. This guarantees that
	// CANCEL / ACK requests sipgo builds from the original INVITE route
	// to the bound socket even though sipgo's SetDestination is not
	// always preserved through dialog state.
	buildReq := func(target ForkTarget) *sip.Request {
		host, port := splitHostPort(target.Socket)
		branchURI := sip.Uri{
			Scheme: recipient.Scheme,
			User:   recipient.User,
			Host:   host,
			Port:   port,
		}
		req := sip.NewRequest(sip.INVITE, branchURI)
		req.SetBody(sdpOffer)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.AppendHeader(e.AllowHeader())
		toURI := recipient
		req.AppendHeader(&sip.ToHeader{Address: toURI})
		if opts.FromUser != "" {
			fromURI := sip.Uri{Scheme: "sip", User: opts.FromUser, Host: e.publicHost}
			from := &sip.FromHeader{Address: fromURI}
			from.Params.Add("tag", sip.GenerateTagN(16))
			req.AppendHeader(from)
			req.AppendHeader(sip.NewHeader("P-Asserted-Identity", fromURI.String()))
		}
		for _, h := range opts.Headers {
			req.AppendHeader(h)
		}
		return req
	}

	branchCtx, branchCancel := context.WithCancelCause(ctx)

	var (
		winnerMu       sync.Mutex
		winner         *sipgo.DialogClientSession
		earlyMediaOnce sync.Once
	)

	type branchResult struct {
		ds         *sipgo.DialogClientSession
		err        error
		statusCode int
	}
	results := make(chan branchResult, len(opts.ForkTargets))

	for i, target := range opts.ForkTargets {
		i, target := i, target
		go func() {
			req := buildReq(target)
			if target.Transport != "" {
				req.SetTransport(strings.ToUpper(target.Transport))
			}
			e.logSIPMessage("outbound", req)

			ds, werr := e.dcCache.WriteInvite(branchCtx, req)
			if werr != nil {
				e.log.Info("fork branch write failed", "i", i, "target", target.Socket, "error", werr)
				results <- branchResult{nil, werr, 0}
				return
			}

			answerOpts := sipgo.AnswerOptions{
				Username: opts.AuthUsername,
				Password: opts.AuthPassword,
				OnResponse: func(res *sip.Response) error {
					e.logSIPMessage("inbound", res)
					if opts.OnEarlyMedia == nil || res.StatusCode != sip.StatusSessionInProgress {
						return nil
					}
					body := res.Body()
					if len(body) == 0 {
						return nil
					}
					earlyMediaOnce.Do(func() {
						remoteSDP, perr := ParseSDP(body)
						if perr != nil {
							e.log.Warn("fork early media: parse 183 SDP", "error", perr)
							return
						}
						if serr := rtpSess.SetRemote(remoteSDP.RemoteIP, remoteSDP.RemotePort); serr != nil {
							e.log.Warn("fork early media: set remote", "error", serr)
							return
						}
						if len(remoteSDP.Codecs) > 0 {
							rtpSess.SendKeepalive(remoteSDP.Codecs[0].PayloadType(), 3)
						}
						opts.OnEarlyMedia(remoteSDP, rtpSess)
					})
					return nil
				},
			}

			aerr := ds.WaitAnswer(branchCtx, answerOpts)
			if aerr != nil {
				code := 0
				if ds.InviteResponse != nil {
					code = ds.InviteResponse.StatusCode
				}
				results <- branchResult{ds, aerr, code}
				return
			}

			winnerMu.Lock()
			if winner == nil {
				winner = ds
				winnerMu.Unlock()
				// Plain cancel (no WaitAnswerForceCancelErr cause) so sipgo
				// actually sends CANCEL on the losing branches instead of
				// exiting WaitAnswer without it.
				branchCancel(errBranchLost)
				results <- branchResult{ds, nil, ds.InviteResponse.StatusCode}
				return
			}
			winnerMu.Unlock()

			// Lost the race after our own 2xx arrived — finish the dialog
			// cleanly so the peer doesn't keep a half-open call.
			_ = ds.Ack(context.Background())
			_ = ds.Bye(context.Background())
			results <- branchResult{ds, fmt.Errorf("lost race"), 200}
		}()
	}

	var (
		bestStatus int
		lastErr    error
	)
	for i := 0; i < len(opts.ForkTargets); i++ {
		r := <-results
		if r.err == nil {
			continue
		}
		lastErr = r.err
		if r.statusCode > bestStatus {
			bestStatus = r.statusCode
		}
	}
	branchCancel(nil)

	if winner == nil {
		rtpSess.Close()
		if bestStatus > 0 {
			return nil, fmt.Errorf("all INVITE branches failed (best status %d): %w", bestStatus, lastErr)
		}
		if lastErr != nil {
			return nil, fmt.Errorf("all INVITE branches failed: %w", lastErr)
		}
		return nil, fmt.Errorf("all INVITE branches failed")
	}

	ds := winner
	if err := ds.Ack(ctx); err != nil {
		ds.Bye(context.Background())
		rtpSess.Close()
		return nil, fmt.Errorf("ack: %w", err)
	}

	remoteSDP, err := ParseSDP(ds.InviteResponse.Body())
	if err != nil {
		ds.Bye(context.Background())
		rtpSess.Close()
		return nil, fmt.Errorf("parse answer SDP: %w", err)
	}
	if err := rtpSess.SetRemote(remoteSDP.RemoteIP, remoteSDP.RemotePort); err != nil {
		ds.Bye(context.Background())
		rtpSess.Close()
		return nil, fmt.Errorf("set remote: %w", err)
	}
	if len(remoteSDP.Codecs) > 0 {
		rtpSess.SendKeepalive(remoteSDP.Codecs[0].PayloadType(), 3)
	}

	return &OutboundCall{
		Dialog:    ds,
		RemoteSDP: remoteSDP,
		RTPSess:   rtpSess,
	}, nil
}
