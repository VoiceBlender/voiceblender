package sip

import "testing"

func TestGenerateDTMFPacketsCount(t *testing.T) {
	pkts := GenerateDTMFPackets('5', 101, 0x1234, 0, 0, 160)
	if len(pkts) != 7 {
		t.Fatalf("packet count = %d, want 7", len(pkts))
	}
	if !pkts[0].Header.Marker {
		t.Errorf("first packet missing marker bit")
	}
	for i, p := range pkts {
		if p.Header.PayloadType != 101 {
			t.Errorf("packet[%d] PT = %d, want 101", i, p.Header.PayloadType)
		}
		if i > 0 && p.Header.Marker {
			t.Errorf("packet[%d] unexpectedly has marker bit", i)
		}
	}
}

func TestGenerateDTMFPacketsDurationUnits(t *testing.T) {
	cases := []struct {
		name          string
		samplesPerPkt uint16
		wantDurations []uint16 // durations of the 4 normal packets
		wantFinal     uint16
	}{
		{"8kHz", 160, []uint16{160, 320, 480, 640}, 640},
		{"AMRWB-16kHz", 320, []uint16{320, 640, 960, 1280}, 1280},
		{"zero-defaults-to-8kHz", 0, []uint16{160, 320, 480, 640}, 640},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pkts := GenerateDTMFPackets('7', 101, 1, 0, 0, c.samplesPerPkt)
			if len(pkts) != 7 {
				t.Fatalf("packet count = %d, want 7", len(pkts))
			}
			// 4 normal event packets, durations ramping up.
			for i := 0; i < 4; i++ {
				ev, err := DecodeDTMFEvent(pkts[i].Payload)
				if err != nil {
					t.Fatalf("decode packet[%d]: %v", i, err)
				}
				if ev.EndOfEvent {
					t.Errorf("packet[%d] should not be end-of-event", i)
				}
				if ev.Duration != c.wantDurations[i] {
					t.Errorf("packet[%d] duration = %d, want %d", i, ev.Duration, c.wantDurations[i])
				}
			}
			// 3 end-of-event packets, all with the final duration.
			for i := 4; i < 7; i++ {
				ev, err := DecodeDTMFEvent(pkts[i].Payload)
				if err != nil {
					t.Fatalf("decode packet[%d]: %v", i, err)
				}
				if !ev.EndOfEvent {
					t.Errorf("packet[%d] should be end-of-event", i)
				}
				if ev.Duration != c.wantFinal {
					t.Errorf("packet[%d] final duration = %d, want %d", i, ev.Duration, c.wantFinal)
				}
			}
		})
	}
}
