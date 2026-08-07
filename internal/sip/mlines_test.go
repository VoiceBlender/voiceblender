package sip

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func audioSlot(mid string, port int, dir string) MLineSlot {
	return MLineSlot{
		Media:    "audio",
		Proto:    []string{"RTP", "AVP"},
		MID:      mid,
		State:    SlotActive,
		StreamID: mid,
		Local: AudioStream{
			Port:      port,
			Direction: dir,
			Codecs:    []codec.CodecType{codec.CodecPCMU},
		},
	}
}

func TestSlotTable_AppendNeverShrinks(t *testing.T) {
	var tb MLineTable

	if got := tb.Append(audioSlot("0", 40000, DirSendRecv)); got != 0 {
		t.Fatalf("first Append = %d, want 0", got)
	}
	if got := tb.Append(audioSlot("1", 40002, DirSendOnly)); got != 1 {
		t.Fatalf("second Append = %d, want 1", got)
	}

	tb.Tombstone(1)
	if tb.Len() != 2 {
		t.Errorf("Len after Tombstone = %d, want 2 — RFC 3264 §8 forbids shrinking", tb.Len())
	}

	// A new stream must take a fresh slot, not reclaim the tombstone's index,
	// so the peer's positional matching stays valid.
	if got := tb.Append(audioSlot("2", 40004, DirSendOnly)); got != 2 {
		t.Errorf("third Append = %d, want 2", got)
	}
	if tb.Len() != 3 {
		t.Errorf("Len = %d, want 3", tb.Len())
	}
}

func TestSlotTable_TombstonePersists(t *testing.T) {
	var tb MLineTable
	tb.Append(audioSlot("0", 40000, DirSendRecv))
	tb.Append(audioSlot("1", 40002, DirSendOnly))
	tb.Tombstone(1)

	s := tb.Slot(1)
	if s.State != SlotTombstone {
		t.Errorf("State = %v, want tombstone", s.State)
	}
	if s.Local.Port != 0 {
		t.Errorf("Local.Port = %d, want 0", s.Local.Port)
	}
	if s.StreamID != "" {
		t.Errorf("StreamID = %q, want cleared", s.StreamID)
	}
	if s.MID != "1" {
		t.Errorf("MID = %q, want the slot to keep its identity", s.MID)
	}

	streams := tb.LocalStreams(false, nil)
	if len(streams) != 2 {
		t.Fatalf("LocalStreams len = %d, want 2", len(streams))
	}
	if streams[1].Port != 0 {
		t.Errorf("tombstoned slot rendered with port %d, want 0", streams[1].Port)
	}
}

func TestSlotTable_Lookups(t *testing.T) {
	var tb MLineTable
	tb.Append(audioSlot("orig", 40000, DirSendRecv))
	tb.Append(audioSlot("xlat", 40002, DirSendOnly))

	if s, ok := tb.ByMID("xlat"); !ok || s.Local.Port != 40002 {
		t.Errorf("ByMID(xlat) = %+v ok=%v", s, ok)
	}
	if _, ok := tb.ByMID("nope"); ok {
		t.Error("ByMID must miss on an unknown mid")
	}
	if _, ok := tb.ByMID(""); ok {
		t.Error("ByMID must miss on an empty mid")
	}
	if s, ok := tb.ByStreamID("orig"); !ok || s.Index != 0 {
		t.Errorf("ByStreamID(orig) = %+v ok=%v", s, ok)
	}
	if _, ok := tb.ByStreamID(""); ok {
		t.Error("ByStreamID must miss on an empty id")
	}
	if tb.Slot(-1) != nil || tb.Slot(2) != nil {
		t.Error("Slot must return nil out of range")
	}
}

func TestSlotTable_ActiveAudio(t *testing.T) {
	var tb MLineTable
	tb.Append(audioSlot("0", 40000, DirSendRecv))
	tb.Append(MLineSlot{Media: "text", State: SlotActive})
	tb.Append(audioSlot("2", 40004, DirSendOnly))
	tb.Tombstone(2)

	active := tb.ActiveAudio()
	if len(active) != 1 || active[0].MID != "0" {
		t.Errorf("ActiveAudio = %d slots, want just the first audio slot", len(active))
	}

	pending := tb.Append(audioSlot("3", 40006, DirSendOnly))
	tb.Slot(pending).State = SlotPending
	if got := tb.ActiveAudioCount(); got != 2 {
		t.Errorf("ActiveAudioCount = %d, want 2 (active + pending, excluding the tombstone)", got)
	}
}

func TestSlotTable_MintMIDAvoidsCollisions(t *testing.T) {
	var tb MLineTable

	if got := tb.MintMID(); got != "0" {
		t.Errorf("first MintMID = %q, want 0", got)
	}
	tb.Append(audioSlot("1", 40002, DirSendRecv))
	// "1" is taken by a peer-supplied mid, so minting must skip past it.
	if got := tb.MintMID(); got == "1" {
		t.Error("MintMID returned a mid already in the table")
	}

	var tb2 MLineTable
	tb2.Append(audioSlot("7", 40000, DirSendRecv))
	tb2.ReserveMID("7")
	if got := tb2.MintMID(); got != "8" {
		t.Errorf("MintMID after ReserveMID(7) = %q, want 8", got)
	}
}

func TestLocalStreams_AppliesHoldTransform(t *testing.T) {
	var tb MLineTable
	tb.Append(audioSlot("0", 40000, DirSendRecv))
	tb.Append(audioSlot("1", 40002, DirSendOnly))
	tb.Append(audioSlot("2", 40004, DirRecvOnly))

	off := tb.LocalStreams(false, nil)
	if off[0].Direction != DirSendRecv || off[1].Direction != DirSendOnly || off[2].Direction != DirRecvOnly {
		t.Errorf("off hold = %q/%q/%q, want the desired directions unchanged",
			off[0].Direction, off[1].Direction, off[2].Direction)
	}

	on := tb.LocalStreams(true, nil)
	if on[0].Direction != DirSendOnly {
		t.Errorf("held sendrecv = %q, want sendonly", on[0].Direction)
	}
	if on[1].Direction != DirSendOnly {
		t.Errorf("held sendonly = %q, want sendonly", on[1].Direction)
	}
	if on[2].Direction != DirInactive {
		t.Errorf("held recvonly = %q, want inactive", on[2].Direction)
	}

	// Hold must not mutate intent, or unhold would resurrect the wrong direction.
	if tb.Slot(1).Local.Direction != DirSendOnly || tb.Slot(2).Local.Direction != DirRecvOnly {
		t.Error("LocalStreams mutated the stored desired direction")
	}
}

func TestLocalStreams_CarriesIdentityAttributes(t *testing.T) {
	var tb MLineTable
	i := tb.Append(audioSlot("xlat", 40002, DirSendOnly))
	s := tb.Slot(i)
	s.Label = "2"
	s.Content = "alt"
	s.Lang = "es"

	got := tb.LocalStreams(false, nil)[0]
	if got.MID != "xlat" || got.Label != "2" || got.Content != "alt" || got.Lang != "es" {
		t.Errorf("identity attributes not carried: %+v", got)
	}
}

func TestLocalStreams_SkipMedia(t *testing.T) {
	var tb MLineTable
	tb.Append(audioSlot("0", 40000, DirSendRecv))
	tb.Append(MLineSlot{Media: "text", State: SlotActive})
	tb.Append(audioSlot("2", 40004, DirSendOnly))

	// The caller renders its own m=text section, so that slot is skipped here.
	got := tb.LocalStreams(false, map[int]bool{1: true})
	if len(got) != 2 {
		t.Fatalf("LocalStreams len = %d, want 2", len(got))
	}
	if got[0].Port != 40000 || got[1].Port != 40004 {
		t.Errorf("ports = %d/%d, want 40000/40004", got[0].Port, got[1].Port)
	}
}

func TestSlotStateString(t *testing.T) {
	for state, want := range map[SlotState]string{
		SlotPending:   "pending",
		SlotActive:    "active",
		SlotTombstone: "removed",
	} {
		if got := state.String(); got != want {
			t.Errorf("SlotState(%d).String() = %q, want %q", state, got, want)
		}
	}
}
