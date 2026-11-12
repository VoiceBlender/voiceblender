package freeswitch

import (
	"crypto/tls"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderConfig_IsWellFormedAndCarriesSettings(t *testing.T) {
	s := DefaultSettings()
	out, err := renderConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	// X-PRE-PROCESS is FreeSWITCH syntax, but still well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(string(out)))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("rendered config is not well-formed XML: %v", err)
			}
			break
		}
	}
	for _, want := range []string{
		`name="sip-port" value="7060"`,
		`name="tls-sip-port" value="7061"`,
		`name="sip-port" value="7062"`,
		`name="sip-port" value="7063"`,
		`name="listen-port" value="7021"`,
		`name="rtp-start-port" value="7100"`,
		`name="rtp-timer-name" value="soft"`,
		`<user id="1009">`,
		`sofia/interop-flap/sip:tone-$1@127.0.0.1:7060`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered config lacks %s", want)
		}
	}
}

// Every profile needs the soft timer: without it FreeSWITCH blocks in media
// reads and handles a BYE only when the next RTCP packet arrives.
func TestRenderConfig_EveryProfileHasRTPTimer(t *testing.T) {
	out, err := renderConfig(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	profiles := strings.Count(string(out), "<profile name=\"interop")
	timers := strings.Count(string(out), `name="rtp-timer-name" value="soft"`)
	if profiles != 3 || timers != profiles {
		t.Errorf("%d sofia profiles, %d with rtp-timer-name", profiles, timers)
	}
}

func TestSettingsPortsStayInPeerBand(t *testing.T) {
	s := DefaultSettings()
	for _, p := range append(s.Ports(), s.RTPMin, s.RTPMax) {
		if p < 6000 || p > 8191 {
			t.Errorf("port %d outside the interop peer band 6000-8191", p)
		}
	}
}

func TestWriteConfDir(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfDir(dir, DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"freeswitch.xml", "tls/agent.pem", "tls/cafile.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "gateways")); err != nil || !fi.IsDir() {
		t.Errorf("gateways dir missing: %v", err)
	}
	agent, err := os.ReadFile(filepath.Join(dir, "tls/agent.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(agent, agent); err != nil {
		t.Errorf("agent.pem is not a usable cert+key: %v", err)
	}
}

func TestRenderGateway(t *testing.T) {
	out, err := renderGateway(gatewaySpec{Name: "gw1", Registrar: "127.0.0.1:8200", Domain: "127.0.0.1", User: "alice", Password: "pw", Expires: 60})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<gateway name="gw1">`, `name="proxy" value="127.0.0.1:8200"`, `name="username" value="alice"`, `name="expire-seconds" value="60"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("gateway lacks %s:\n%s", want, out)
		}
	}
}

func TestCodecString(t *testing.T) {
	cases := []struct {
		codecs []string
		ptime  int
		want   string
	}{
		{[]string{"PCMU"}, 0, "PCMU"},
		{[]string{"opus", "g722"}, 0, "OPUS,G722"},
		{[]string{"PCMA"}, 30, "PCMA@30i"},
	}
	for _, c := range cases {
		if got := codecString(c.codecs, c.ptime); got != c.want {
			t.Errorf("codecString(%v, %d) = %q, want %q", c.codecs, c.ptime, got, c.want)
		}
	}
}

func TestHangupSIPCode(t *testing.T) {
	cases := []struct {
		ev   map[string]string
		want int
	}{
		{map[string]string{"variable_proto_specific_hangup_cause": "sip:486"}, 486},
		{map[string]string{"variable_sip_term_status": "200"}, 200},
		{map[string]string{"variable_sip_hangup_disposition": "send_refuse", "Hangup-Cause": "CALL_REJECTED"}, 603},
		{map[string]string{"variable_sip_hangup_disposition": "send_refuse", "Hangup-Cause": "UNALLOCATED_NUMBER"}, 404},
		{map[string]string{"variable_sip_hangup_disposition": "recv_bye"}, 0},
	}
	for _, c := range cases {
		if got := hangupSIPCode(c.ev); got != c.want {
			t.Errorf("hangupSIPCode(%v) = %d, want %d", c.ev, got, c.want)
		}
	}
}
