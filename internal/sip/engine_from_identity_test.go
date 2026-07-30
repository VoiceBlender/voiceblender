package sip

import (
	"testing"
)

// engineFromIdentityPublicHost is deliberately different from every FromHost
// used below, so an implementation that ignores InviteOptions.FromHost and
// always reaches for the engine's publicHost cannot pass by coincidence.
const engineFromIdentityPublicHost = "vb.public.example"

func TestEngineFromIdentity(t *testing.T) {
	e := &Engine{publicHost: engineFromIdentityPublicHost}

	t.Run("no caller id", func(t *testing.T) {
		from, pai := e.fromIdentity(InviteOptions{})
		if from != nil || pai != nil {
			t.Fatalf("want (nil, nil) with no FromUser, got from=%v pai=%v", from, pai)
		}
	})

	t.Run("default host", func(t *testing.T) {
		from, _ := e.fromIdentity(InviteOptions{FromUser: "alice"})
		if from == nil {
			t.Fatal("want a From header, got nil")
		}
		if got := from.Address.Host; got != engineFromIdentityPublicHost {
			t.Errorf("From host = %q, want the engine publicHost %q", got, engineFromIdentityPublicHost)
		}
		if got := from.Address.User; got != "alice" {
			t.Errorf("From user = %q, want %q", got, "alice")
		}
	})

	t.Run("explicit host", func(t *testing.T) {
		from, _ := e.fromIdentity(InviteOptions{FromUser: "alice", FromHost: "pbx.example.com"})
		if from == nil {
			t.Fatal("want a From header, got nil")
		}
		if got := from.Address.Host; got != "pbx.example.com" {
			t.Errorf("From host = %q, want the FromHost override %q", got, "pbx.example.com")
		}
	})

	t.Run("pai tracks from", func(t *testing.T) {
		from, pai := e.fromIdentity(InviteOptions{FromUser: "alice", FromHost: "pbx.example.com"})
		if from == nil || pai == nil {
			t.Fatal("want both headers, got nil")
		}
		const want = "sip:alice@pbx.example.com"
		if got := pai.Value(); got != want {
			t.Errorf("P-Asserted-Identity = %q, want %q (must track the From URI)", got, want)
		}
	})

	t.Run("from tag present", func(t *testing.T) {
		from, _ := e.fromIdentity(InviteOptions{FromUser: "alice", FromHost: "pbx.example.com"})
		if from == nil {
			t.Fatal("want a From header, got nil")
		}
		tag, ok := from.Params.Get("tag")
		if !ok || tag == "" {
			t.Errorf("From tag = %q (present=%v), want a non-empty tag", tag, ok)
		}
	})
}
