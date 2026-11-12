package freeswitch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

type call struct {
	p    *Peer
	uuid string
	tag  string

	mu       sync.Mutex
	vars     map[string]string
	counts   map[peers.EventKind]int
	used     map[peers.EventKind]int
	dtmf     strings.Builder
	hungUp   bool
	hangup   peers.HangupInfo
	jobErr   string
	recSeq   int
	notify   chan struct{}
	evLog    []string
	answered bool
}

func (c *call) ID() string { return c.uuid }

func (c *call) handle(name string, ev map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range ev {
		if s, ok := strings.CutPrefix(k, "variable_"); ok {
			c.vars[s] = v
		}
	}
	c.evLog = append(c.evLog, name)
	switch name {
	case "CHANNEL_ANSWER":
		c.answered = true
		c.counts[peers.EventAnswered]++
	case "CHANNEL_PROGRESS_MEDIA":
		c.counts[peers.EventEarlyMedia]++
	case "CHANNEL_HOLD":
		c.counts[peers.EventHeld]++
	case "CHANNEL_UNHOLD":
		c.counts[peers.EventUnheld]++
	case "CALL_UPDATE":
		c.counts[peers.EventReInvite]++
	case "RECV_RTCP_MESSAGE":
		c.counts[peers.EventRTCP]++
	case "DTMF":
		c.dtmf.WriteString(ev["DTMF-Digit"])
	case "CHANNEL_HANGUP_COMPLETE":
		c.markHungUpLocked(peers.HangupInfo{
			Cause:       ev["Hangup-Cause"],
			SIPCode:     hangupSIPCode(ev),
			Disposition: ev["variable_sip_hangup_disposition"],
		})
	}
	c.wakeLocked()
}

func (c *call) markHungUpLocked(info peers.HangupInfo) {
	if c.hungUp {
		return
	}
	c.hungUp = true
	c.hangup = info
	c.counts[peers.EventHangup]++
}

func (c *call) wakeLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

// jobResult receives the outcome of the bgapi originate. A failure that never
// produced a channel would otherwise leave WaitHangup waiting forever.
func (c *call) jobResult(body string) {
	if !strings.HasPrefix(body, "-ERR") {
		return
	}
	cause := strings.TrimSpace(strings.TrimPrefix(body, "-ERR"))
	time.AfterFunc(2*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.jobErr = cause
		c.markHungUpLocked(peers.HangupInfo{Cause: cause})
		c.wakeLocked()
	})
}

func (c *call) isHungUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hungUp
}

func (c *call) wait(ctx context.Context, what string, cond func() bool) error {
	for {
		c.mu.Lock()
		if cond() {
			c.mu.Unlock()
			return nil
		}
		ch := c.notify
		log := strings.Join(c.evLog, ",")
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("freeswitch call %s: waiting for %s: %w (events: %s)", c.uuid, what, ctx.Err(), log)
		}
	}
}

func (c *call) uuidAPI(cmd string, args ...string) error {
	_, err := c.p.api(strings.Join(append([]string{cmd, c.uuid}, args...), " "))
	return err
}

func (c *call) Ring() error { return c.uuidAPI("uuid_ring_ready") }

func (c *call) EarlyMedia(toneHz int) error {
	if err := c.uuidAPI("uuid_pre_answer"); err != nil {
		return err
	}
	if toneHz > 0 {
		return c.PlayTone(toneHz)
	}
	return nil
}

func (c *call) Answer(o peers.AnswerOpts) error {
	if o.Codec != "" {
		if err := c.uuidAPI("uuid_setvar", "absolute_codec_string", codecString([]string{o.Codec}, o.Ptime)); err != nil {
			return err
		}
	}
	if o.NoSRTP {
		if err := c.uuidAPI("uuid_setvar", "rtp_secure_media", "forbidden"); err != nil {
			return err
		}
	}
	return c.uuidAPI("uuid_answer")
}

func (c *call) Reject(sipCode int) error { return c.uuidAPI("uuid_kill", peers.SIPCodeToQ850(sipCode)) }

func (c *call) Hangup() error { return c.uuidAPI("uuid_kill", "NORMAL_CLEARING") }

func (c *call) PlayTone(hz int) error {
	return c.uuidAPI("uuid_broadcast", fmt.Sprintf("tone_stream://%%(1000,0,%d);loops=-1", hz), "aleg")
}

func (c *call) StopMedia() error { return c.uuidAPI("uuid_break", "all") }

func (c *call) Echo() error { return c.uuidAPI("uuid_transfer", "echo", "inline") }

func (c *call) StartRecording() (func() (string, error), error) {
	c.mu.Lock()
	c.recSeq++
	name := fmt.Sprintf("%s-%d.wav", c.uuid, c.recSeq)
	c.mu.Unlock()
	ctrPath := recMount + "/" + name
	if err := c.uuidAPI("uuid_setvar", "RECORD_READ_ONLY", "true"); err != nil {
		return nil, err
	}
	if err := c.uuidAPI("uuid_record", "start", ctrPath); err != nil {
		return nil, err
	}
	host := filepath.Join(c.p.recDir, name)
	return func() (string, error) {
		c.uuidAPI("uuid_record", "stop", ctrPath)
		return host, waitFileStable(host, 5*time.Second)
	}, nil
}

func waitFileStable(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := int64(-1)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil {
			if fi.Size() == last && fi.Size() > 44 {
				return nil
			}
			last = fi.Size()
		}
		time.Sleep(150 * time.Millisecond)
	}
	if last < 0 {
		return fmt.Errorf("freeswitch: recording %s never appeared", path)
	}
	return nil
}

func (c *call) SendDTMF(digits string) error { return c.uuidAPI("uuid_send_dtmf", digits+"@120") }

func (c *call) WaitDTMF(ctx context.Context, n int) (string, error) {
	err := c.wait(ctx, fmt.Sprintf("%d DTMF digits", n), func() bool { return c.dtmf.Len() >= n })
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dtmf.String(), err
}

func (c *call) Hold() error   { return c.uuidAPI("uuid_hold") }
func (c *call) Unhold() error { _, err := c.p.api("uuid_hold off " + c.uuid); return err }

func (c *call) Renegotiate(codec string, ptime int) error {
	return c.uuidAPI("uuid_media_reneg", "="+codecString([]string{codec}, ptime))
}

// DropSRTP is unsupported: FreeSWITCH keeps RTP/SAVP in a re-offer even with
// SRTP forbidden, then hangs up on its own answer.
func (c *call) DropSRTP() error { return peers.ErrUnsupported }

func (c *call) Transport() string { return strings.ToLower(c.Var("sip_via_protocol")) }

// Refer runs in the background: uuid_deflect blocks until the REFER
// transaction ends, which a peer may hold open while it decides.
func (c *call) Refer(target string) error { return c.p.bgapi("uuid_deflect " + c.uuid + " " + target) }

func (c *call) ReferStatus(ctx context.Context) (int, error) {
	var code int
	// sip_refer_status_code holds the REFER's final response and fires no
	// event, so it is polled. A deflected call is hung up on the 202, before
	// any NOTIFY, so an accepted REFER's sipfrag outcome is never visible.
	final := func(n int) bool { return n >= 200 && n != 202 }
	for {
		code = atoi(c.Var("sip_refer_status_code"))
		if final(code) {
			return code, nil
		}
		if c.isHungUp() {
			return code, peers.ErrUnsupported
		}
		select {
		case <-ctx.Done():
			return code, fmt.Errorf("freeswitch call %s: no REFER outcome: %w", c.uuid, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *call) WaitAnswered(ctx context.Context) error {
	err := c.wait(ctx, "answer", func() bool { return c.answered || c.hungUp })
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.answered {
		return fmt.Errorf("freeswitch call %s ended before answer: %s (SIP %d)", c.uuid, c.hangup.Cause, c.hangup.SIPCode)
	}
	return nil
}

func (c *call) WaitHangup(ctx context.Context) (peers.HangupInfo, error) {
	err := c.wait(ctx, "hangup", func() bool { return c.hungUp })
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hangup, err
}

func (c *call) WaitEvent(ctx context.Context, kind peers.EventKind) error {
	err := c.wait(ctx, string(kind), func() bool { return c.counts[kind] > c.used[kind] })
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.used[kind]++
	c.mu.Unlock()
	return nil
}

func (c *call) Header(name string) string { return c.Var("sip_h_" + name) }

// Var reads a channel variable live, falling back to the last value an event
// carried once the channel is gone.
func (c *call) Var(name string) string {
	if !c.isHungUp() {
		if out, err := c.p.api(fmt.Sprintf("uuid_getvar %s %s", c.uuid, name)); err == nil && out != "_undef_" {
			return out
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vars[name]
}

// Codec reads rtp_use_codec_name, which follows renegotiation; read_codec
// keeps the codec the channel started with.
func (c *call) Codec() string {
	v := c.Var("rtp_use_codec_name")
	if v == "" {
		v = c.Var("read_codec")
	}
	return strings.ToUpper(v)
}

func (c *call) RemoteSDP() string { return c.Var("switch_r_sdp") }

func (c *call) Ptime() int { return atoi(c.Var("rtp_use_codec_ptime")) }

func (c *call) SRTP() bool {
	v := c.Var("rtp_has_crypto")
	return v != "" && v != "false"
}

func (c *call) RTCPStats() (peers.RTCPStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return peers.RTCPStats{Reports: c.counts[peers.EventRTCP]}, nil
}

// Events returns the names of the events seen on the call, for diagnostics.
func (c *call) Events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.evLog...)
}

// hangupSIPCode is the final status FreeSWITCH sent or received. A response
// it received is in proto_specific_hangup_cause ("sip:486"); for one it sent
// while refusing a call it records only the cause, so the code is derived
// with the same table sofia uses to pick it.
func hangupSIPCode(ev map[string]string) int {
	if v, ok := strings.CutPrefix(ev["variable_proto_specific_hangup_cause"], "sip:"); ok {
		if n := atoi(v); n > 0 {
			return n
		}
	}
	if n := atoi(ev["variable_sip_term_status"]); n > 0 {
		return n
	}
	if ev["variable_sip_hangup_disposition"] == "send_refuse" {
		return q850ToSIP[ev["Hangup-Cause"]]
	}
	return 0
}

var q850ToSIP = map[string]int{
	"UNALLOCATED_NUMBER":       404,
	"NO_ROUTE_DESTINATION":     404,
	"USER_BUSY":                486,
	"NO_USER_RESPONSE":         408,
	"NO_ANSWER":                480,
	"CALL_REJECTED":            603,
	"INCOMPATIBLE_DESTINATION": 488,
	"NORMAL_TEMPORARY_FAILURE": 503,
	"ORIGINATOR_CANCEL":        487,
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

var _ peers.Call = (*call)(nil)
var _ peers.Peer = (*Peer)(nil)
