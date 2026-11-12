// Package freeswitch runs FreeSWITCH in docker as an interop peer and drives
// it over the event socket.
package freeswitch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

const (
	// EnvImage overrides the docker image.
	EnvImage = "VB_INTEROP_FREESWITCH_IMAGE"
	// EnvReuse keeps the container after the run and reuses a running one on
	// the next, which saves the startup on every iteration.
	EnvReuse = "VB_INTEROP_FREESWITCH_REUSE"
	// EnvLogLevel sets the FreeSWITCH log level.
	EnvLogLevel = "VB_INTEROP_FREESWITCH_LOGLEVEL"
	// EnvSIPTrace logs every SIP message FreeSWITCH sends and receives.
	EnvSIPTrace = "VB_INTEROP_FREESWITCH_SIPTRACE"

	// DefaultImage is FreeSWITCH 1.11.3, pinned by digest.
	DefaultImage = "safarov/freeswitch@sha256:f156e01a96d48944078ed7f0e24304be63ea15f6466cf607a0aeb6cb47761da7"

	containerName = "vb-interop-freeswitch"
	profile       = "interop"
	authProfile   = "interop-auth"
	flapProfile   = "interop-flap"
	recMount      = "/recordings"
	logMount      = "/var/log/freeswitch"
)

func init() {
	peers.Register("freeswitch", func() (peers.Peer, error) {
		p, err := Start(DefaultSettings())
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}

// Peer is a running FreeSWITCH.
type Peer struct {
	s         Settings
	container *peers.Container
	reuse     bool
	baseDir   string
	confDir   string
	recDir    string
	logDir    string

	cmd *eslConn
	ev  *eslConn

	mu     sync.Mutex
	calls  map[string]*call
	byTag  map[string][]*call
	jobs   map[string]*call
	notify chan struct{}
	closed bool
}

// Start brings up FreeSWITCH in docker and connects to its event socket.
func Start(s Settings) (*Peer, error) {
	if lvl := os.Getenv(EnvLogLevel); lvl != "" {
		s.LogLevel = lvl
	}
	if err := peers.DockerAvailable(); err != nil {
		return nil, err
	}
	p := &Peer{
		s:      s,
		reuse:  os.Getenv(EnvReuse) != "",
		calls:  map[string]*call{},
		byTag:  map[string][]*call{},
		jobs:   map[string]*call{},
		notify: make(chan struct{}),
	}

	var err error
	if p.reuse {
		p.baseDir = filepath.Join(os.TempDir(), containerName)
		if err := os.MkdirAll(p.baseDir, 0o755); err != nil {
			return nil, err
		}
	} else if p.baseDir, err = os.MkdirTemp("", containerName+"-"); err != nil {
		return nil, err
	}
	p.confDir = filepath.Join(p.baseDir, "conf")
	p.recDir = filepath.Join(p.baseDir, "rec")
	p.logDir = filepath.Join(p.baseDir, "log")

	if p.reuse && p.confUnchanged() && p.connect(3*time.Second) == nil {
		p.container = &peers.Container{ID: containerName, Name: containerName}
		if err := p.waitProfiles(30 * time.Second); err == nil {
			return p, p.ready()
		}
		p.closeConns()
	}

	peers.RemoveStale(containerName)
	for _, port := range s.Ports() {
		if !portFree(s.IP, port) {
			return nil, fmt.Errorf("freeswitch: port %d on %s is in use; the interop peer band 6000-8191 must be free", port, s.IP)
		}
	}
	if err := os.RemoveAll(p.confDir); err != nil {
		return nil, err
	}
	if err := WriteConfDir(p.confDir, s); err != nil {
		return nil, err
	}
	for _, d := range []string{p.recDir, p.logDir} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return nil, err
		}
		os.Chmod(d, 0o777)
	}

	image := DefaultImage
	if v := os.Getenv(EnvImage); v != "" {
		image = v
	}
	p.container, err = peers.Run(peers.RunSpec{
		Name:    containerName,
		Image:   image,
		Volumes: map[string]string{p.confDir: "/etc/freeswitch", p.recDir: recMount, p.logDir: logMount},
	})
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		if err = p.connect(2 * time.Second); err == nil {
			break
		}
		if time.Now().After(deadline) || !p.container.Running() {
			return nil, p.startFailure(fmt.Errorf("event socket not reachable: %w", err))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := p.waitProfiles(30 * time.Second); err != nil {
		return nil, p.startFailure(err)
	}
	if err := p.ready(); err != nil {
		return nil, p.startFailure(err)
	}
	return p, nil
}

func (p *Peer) ready() error {
	if err := p.subscribe(); err != nil {
		return err
	}
	trace := "off"
	if os.Getenv(EnvSIPTrace) != "" {
		trace = "on"
	}
	_, err := p.api("sofia global siptrace " + trace)
	return err
}

// confUnchanged reports whether a reused container was started with the
// config this build would render.
func (p *Peer) confUnchanged() bool {
	want, err := renderConfig(p.s)
	if err != nil {
		return false
	}
	have, err := os.ReadFile(filepath.Join(p.confDir, "freeswitch.xml"))
	return err == nil && string(have) == string(want)
}

func (p *Peer) startFailure(err error) error {
	logs := p.container.Logs(40) + p.LogTail(80)
	p.Close()
	return fmt.Errorf("freeswitch: %w\n--- log tail ---\n%s", err, logs)
}

// LogPath is the FreeSWITCH log file on the host.
func (p *Peer) LogPath() string { return filepath.Join(p.logDir, "freeswitch.log") }

// LogTail returns the last n lines of the FreeSWITCH log.
func (p *Peer) LogTail(n int) string {
	b, err := os.ReadFile(p.LogPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func portFree(ip string, port int) bool {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (p *Peer) eslAddr() string {
	return net.JoinHostPort(p.s.IP, strconv.Itoa(p.s.ESLPort))
}

func (p *Peer) connect(timeout time.Duration) error {
	cmd, err := dialESL(p.eslAddr(), p.s.ESLPassword, timeout)
	if err != nil {
		return err
	}
	p.cmd = cmd
	return nil
}

func (p *Peer) waitProfiles(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var out string
	for time.Now().Before(deadline) {
		out, _ = p.cmd.api("sofia status")
		ok := true
		for _, port := range []int{p.s.SIPPort, p.s.TLSPort, p.s.AuthPort, p.s.FlapPort} {
			if !strings.Contains(out, fmt.Sprintf("%s:%d\tRUNNING", p.s.IP, port)) {
				ok = false
			}
		}
		if ok {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("sofia profiles not all RUNNING:\n%s", out)
}

func (p *Peer) subscribe() error {
	ev, err := dialESL(p.eslAddr(), p.s.ESLPassword, 5*time.Second)
	if err != nil {
		return err
	}
	if _, err := ev.command("event plain ALL"); err != nil {
		ev.Close()
		return err
	}
	p.ev = ev
	go p.readEvents()
	return nil
}

func (p *Peer) readEvents() {
	for {
		m, err := readESLMessage(p.ev.r)
		if err != nil {
			return
		}
		if m.contentType() != "text/event-plain" {
			continue
		}
		ev, body, err := parsePlainEvent(m.Body)
		if err != nil {
			continue
		}
		p.dispatch(ev, body)
	}
}

func (p *Peer) dispatch(ev map[string]string, body string) {
	name := ev["Event-Name"]
	if name == "BACKGROUND_JOB" {
		p.mu.Lock()
		c := p.jobs[ev["Job-UUID"]]
		delete(p.jobs, ev["Job-UUID"])
		p.mu.Unlock()
		if c != nil {
			c.jobResult(strings.TrimSpace(body))
		}
		return
	}
	uuid := ev["Unique-ID"]
	if uuid == "" {
		return
	}
	p.mu.Lock()
	c := p.calls[uuid]
	if c == nil {
		tag := ev["variable_sip_h_"+peers.TagHeader]
		if tag == "" || name == "CHANNEL_DESTROY" {
			p.mu.Unlock()
			return
		}
		c = p.newCallLocked(uuid, tag)
	}
	p.mu.Unlock()
	c.handle(name, ev)
}

func (p *Peer) newCallLocked(uuid, tag string) *call {
	c := &call{p: p, uuid: uuid, tag: tag, vars: map[string]string{}, counts: map[peers.EventKind]int{}, used: map[peers.EventKind]int{}, notify: make(chan struct{})}
	p.calls[uuid] = c
	p.byTag[tag] = append(p.byTag[tag], c)
	close(p.notify)
	p.notify = make(chan struct{})
	return c
}

func (p *Peer) api(cmd string) (string, error) {
	if p.cmd == nil {
		return "", errors.New("freeswitch: not connected")
	}
	return p.cmd.api(cmd)
}

// bgapi runs cmd in the background, for API commands that block until a SIP
// transaction completes.
func (p *Peer) bgapi(cmd string) error {
	if p.cmd == nil {
		return errors.New("freeswitch: not connected")
	}
	m, err := p.cmd.command("bgapi " + cmd)
	if err != nil {
		return err
	}
	if reply := m.Headers["Reply-Text"]; !strings.HasPrefix(reply, "+OK") {
		return fmt.Errorf("freeswitch: bgapi %s: %s", cmd, reply)
	}
	return nil
}

// API runs a raw FreeSWITCH API command, for diagnostics.
func (p *Peer) API(cmd string) (string, error) { return p.api(cmd) }

func (p *Peer) Name() string { return "freeswitch" }

func (p *Peer) Caps() peers.Caps {
	return peers.Caps{
		Codecs:        []string{"PCMU", "PCMA", "G722", "OPUS"},
		SRTP:          true,
		TLS:           true,
		EarlyMedia:    true,
		Hold:          true,
		Refer:         true,
		SessionTimers: true,
		Reneg:         true,
		RTCPStats:     true,
		// FreeSWITCH matches its send ptime to the remote's a=ptime, and
		// VoiceBlender always advertises 20 ms.
		Ptime:     false,
		Register:  true,
		Registrar: true,
		Flap:      true,
	}
}

func (p *Peer) URI(user string, o peers.URIOpts) string {
	switch {
	case o.TLS:
		return fmt.Sprintf("sips:%s@%s:%d", user, p.s.IP, p.s.TLSPort)
	case o.Auth:
		return fmt.Sprintf("sip:%s@%s:%d", user, p.s.IP, p.s.AuthPort)
	}
	return fmt.Sprintf("sip:%s@%s:%d", user, p.s.IP, p.s.SIPPort)
}

func (p *Peer) ToneURI(hz int) string {
	return fmt.Sprintf("sip:tone-%d@%s:%d", hz, p.s.IP, p.s.SIPPort)
}

func (p *Peer) BridgedToneURI(hz int) string {
	return fmt.Sprintf("sip:bridged-%d@%s:%d", hz, p.s.IP, p.s.SIPPort)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func codecString(codecs []string, ptime int) string {
	parts := make([]string, len(codecs))
	for i, c := range codecs {
		parts[i] = fsCodecName(c)
		if ptime > 0 {
			parts[i] += fmt.Sprintf("@%di", ptime)
		}
	}
	return strings.Join(parts, ",")
}

func fsCodecName(c string) string {
	if strings.EqualFold(c, "opus") {
		return "OPUS"
	}
	return strings.ToUpper(c)
}

func (p *Peer) Originate(ctx context.Context, o peers.OriginateOpts) (peers.Call, error) {
	uuid := newUUID()
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	vars := []string{
		"origination_uuid=" + uuid,
		"sip_h_" + peers.TagHeader + "=" + o.Tag,
		fmt.Sprintf("originate_timeout=%d", int(timeout.Seconds())),
		"origination_caller_id_number=interop",
	}
	if len(o.Codecs) > 0 {
		// "^^:" switches the list separator, since a comma would end the variable.
		vars = append(vars, "absolute_codec_string=^^:"+strings.ReplaceAll(codecString(o.Codecs, o.Ptime), ",", ":"))
	}
	if o.SRTP {
		vars = append(vars, "rtp_secure_media=mandatory:AES_CM_128_HMAC_SHA1_80")
	}
	if o.AuthUser != "" {
		vars = append(vars, "sip_auth_username="+o.AuthUser, "sip_auth_password="+o.AuthPassword)
	}
	if o.SessionTimer > 0 {
		vars = append(vars, fmt.Sprintf("sofia_session_timeout=%d", o.SessionTimer))
	}
	for k, v := range o.Headers {
		vars = append(vars, "sip_h_"+k+"='"+v+"'")
	}
	to := o.To
	if o.TLS && !strings.Contains(to, "transport=") {
		to += ";transport=tls"
	}
	dial := "{" + strings.Join(vars, ",") + "}sofia/" + profile + "/" + to

	p.mu.Lock()
	c := p.newCallLocked(uuid, o.Tag)
	p.mu.Unlock()

	m, err := p.cmd.command("bgapi originate " + dial + " &park()")
	if err != nil {
		return nil, err
	}
	reply := m.Headers["Reply-Text"]
	if !strings.HasPrefix(reply, "+OK") {
		return nil, fmt.Errorf("freeswitch: originate: %s", reply)
	}
	p.mu.Lock()
	p.jobs[m.Headers["Job-UUID"]] = c
	p.mu.Unlock()
	return c, nil
}

func (p *Peer) WaitInbound(ctx context.Context, tag string) (peers.Call, error) {
	for {
		p.mu.Lock()
		if cs := p.byTag[tag]; len(cs) > 0 {
			p.mu.Unlock()
			return cs[0], nil
		}
		ch := p.notify
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("freeswitch: no call tagged %s: %w", tag, ctx.Err())
		}
	}
}

// CallsByTag returns every call carrying tag, in arrival order.
func (p *Peer) CallsByTag(tag string) []peers.Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]peers.Call, 0, len(p.byTag[tag]))
	for _, c := range p.byTag[tag] {
		out = append(out, c)
	}
	return out
}

func (p *Peer) HangupAll(tag string) {
	p.mu.Lock()
	cs := append([]*call(nil), p.byTag[tag]...)
	p.mu.Unlock()
	for _, c := range cs {
		if !c.isHungUp() {
			p.api("uuid_kill " + c.uuid + " NORMAL_CLEARING")
		}
	}
	p.mu.Lock()
	for _, c := range p.byTag[tag] {
		delete(p.calls, c.uuid)
	}
	delete(p.byTag, tag)
	p.mu.Unlock()
}

// registration is a FreeSWITCH gateway registering to VoiceBlender.
type registration struct {
	p    *Peer
	name string
	file string
}

func (p *Peer) Register(ctx context.Context, o peers.RegisterOpts) (peers.Registration, error) {
	if o.Expires == 0 {
		o.Expires = 60
	}
	host, _, _ := net.SplitHostPort(o.Registrar)
	if host == "" {
		host = o.Registrar
	}
	name := "vb-" + newUUID()[:8]
	xml, err := renderGateway(gatewaySpec{Name: name, Registrar: o.Registrar, Domain: host, User: o.User, Password: o.Password, Expires: o.Expires})
	if err != nil {
		return nil, err
	}
	file := filepath.Join(p.confDir, "gateways", name+".xml")
	if err := os.WriteFile(file, xml, 0o644); err != nil {
		return nil, err
	}
	if _, err := p.api("reloadxml"); err != nil {
		return nil, err
	}
	if _, err := p.api("sofia profile " + profile + " rescan"); err != nil {
		return nil, err
	}
	return &registration{p: p, name: name, file: file}, nil
}

func (r *registration) Name() string { return r.name }

// Contact is the contact the gateway registers, which VoiceBlender dials.
func (r *registration) WaitRegistered(ctx context.Context) error {
	var out string
	for {
		out, _ = r.p.api("sofia status gateway " + r.name)
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "State" && f[1] == "REGED" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("freeswitch: gateway %s not registered: %w\n%s", r.name, ctx.Err(), out)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (r *registration) Unregister() error {
	os.Remove(r.file)
	_, err := r.p.api("sofia profile " + profile + " killgw " + r.name)
	return err
}

func (p *Peer) Registrar() peers.RegistrarInfo {
	return peers.RegistrarInfo{
		HostPort:     net.JoinHostPort(p.s.IP, strconv.Itoa(p.s.AuthPort)),
		Domain:       p.s.Domain,
		Users:        p.s.Users,
		FlapHostPort: net.JoinHostPort(p.s.IP, strconv.Itoa(p.s.FlapPort)),
	}
}

func (p *Peer) WaitRegistered(ctx context.Context, user string) (string, error) {
	var out string
	for {
		out, _ = p.api(fmt.Sprintf("sofia_contact %s/%s@%s", authProfile, user, p.s.Domain))
		if out != "" && !strings.HasPrefix(out, "error/") {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("freeswitch: %s not registered: %w (%s)", user, ctx.Err(), out)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (p *Peer) StopFlap() error {
	_, err := p.api("sofia profile " + flapProfile + " stop")
	return err
}

func (p *Peer) StartFlap() error {
	if _, err := p.api("sofia profile " + flapProfile + " start"); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := p.api("sofia status")
		if strings.Contains(out, fmt.Sprintf("%s:%d\tRUNNING", p.s.IP, p.s.FlapPort)) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("freeswitch: flap profile did not restart")
}

func (p *Peer) closeConns() {
	if p.ev != nil {
		p.ev.Close()
		p.ev = nil
	}
	if p.cmd != nil {
		p.cmd.Close()
		p.cmd = nil
	}
}

func (p *Peer) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	if p.reuse {
		p.api("hupall NORMAL_CLEARING")
	}
	p.closeConns()
	if p.reuse || p.container == nil {
		return nil
	}
	err := p.container.Remove()
	os.RemoveAll(p.baseDir)
	return err
}
