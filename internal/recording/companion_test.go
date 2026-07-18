package recording

import (
	"bytes"
	"testing"
)

// slotOf builds a slotBytes-long slot whose every byte is v, so a popped slot
// can be identified by value.
func slotOf(v byte, slotBytes int) []byte {
	return bytes.Repeat([]byte{v}, slotBytes)
}

func TestCompanionBuffer_AppendAndBound(t *testing.T) {
	const slotBytes = 4

	tests := []struct {
		name     string
		maxSlots int
		appends  []byte // one slot appended per value
		wantSize int
		wantTail []byte // expected remaining slot values, oldest first
	}{
		{
			name:     "below bound retains all",
			maxSlots: 4,
			appends:  []byte{1, 2, 3},
			wantSize: 3 * slotBytes,
			wantTail: []byte{1, 2, 3},
		},
		{
			name:     "exactly at bound retains all",
			maxSlots: 3,
			appends:  []byte{1, 2, 3},
			wantSize: 3 * slotBytes,
			wantTail: []byte{1, 2, 3},
		},
		{
			name:     "over bound drops the oldest slot",
			maxSlots: 3,
			appends:  []byte{1, 2, 3, 4},
			wantSize: 3 * slotBytes,
			wantTail: []byte{2, 3, 4},
		},
		{
			name:     "far over bound keeps only the newest slots",
			maxSlots: 2,
			appends:  []byte{1, 2, 3, 4, 5, 6, 7},
			wantSize: 2 * slotBytes,
			wantTail: []byte{6, 7},
		},
		{
			name:     "single slot bound keeps the newest only",
			maxSlots: 1,
			appends:  []byte{1, 2, 3},
			wantSize: slotBytes,
			wantTail: []byte{3},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newCompanionBuffer(slotBytes, tc.maxSlots)
			for _, v := range tc.appends {
				c.append(slotOf(v, slotBytes))
			}

			if got := c.size(); got != tc.wantSize {
				t.Errorf("size = %d, want %d", got, tc.wantSize)
			}
			if got := c.size(); got > tc.maxSlots*slotBytes {
				t.Errorf("size %d exceeds bound %d", got, tc.maxSlots*slotBytes)
			}

			// The retained tail must be the newest slots, oldest first.
			for i, want := range tc.wantTail {
				slot, ok := c.pop()
				if !ok {
					t.Fatalf("pop %d: got !ok, want slot %d", i, want)
				}
				if !bytes.Equal(slot, slotOf(want, slotBytes)) {
					t.Fatalf("pop %d = %v, want slot of %d", i, slot, want)
				}
			}
			if _, ok := c.pop(); ok {
				t.Error("pop after draining the tail: got ok, want !ok")
			}
		})
	}
}

func TestCompanionBuffer_PopOnEmpty(t *testing.T) {
	c := newCompanionBuffer(4, 4)
	slot, ok := c.pop()
	if ok {
		t.Errorf("pop on empty = (%v, true), want (nil, false)", slot)
	}
}

func TestCompanionBuffer_PopUnderflowKeepsPartialSlot(t *testing.T) {
	const slotBytes = 4
	c := newCompanionBuffer(slotBytes, 4)

	// A partial slot is not poppable...
	c.append([]byte{1, 1})
	if _, ok := c.pop(); ok {
		t.Fatal("pop with a partial slot buffered: got ok, want !ok")
	}
	if got := c.size(); got != 2 {
		t.Fatalf("size after failed pop = %d, want 2 (partial slot retained)", got)
	}

	// ...until the rest of it arrives, and it must not be lost.
	c.append([]byte{2, 2})
	slot, ok := c.pop()
	if !ok {
		t.Fatal("pop after the slot completed: got !ok, want ok")
	}
	if !bytes.Equal(slot, []byte{1, 1, 2, 2}) {
		t.Errorf("pop = %v, want [1 1 2 2] (partial slot must reassemble)", slot)
	}
}

func TestCompanionBuffer_PopAdvancesOneSlotAtATime(t *testing.T) {
	const slotBytes = 4
	c := newCompanionBuffer(slotBytes, 8)
	c.append(slotOf(1, slotBytes))
	c.append(slotOf(2, slotBytes))

	slot, ok := c.pop()
	if !ok || !bytes.Equal(slot, slotOf(1, slotBytes)) {
		t.Fatalf("first pop = (%v, %v), want (slot of 1, true)", slot, ok)
	}
	if got := c.size(); got != slotBytes {
		t.Errorf("size after one pop = %d, want %d (exactly one slot consumed)", got, slotBytes)
	}

	slot, ok = c.pop()
	if !ok || !bytes.Equal(slot, slotOf(2, slotBytes)) {
		t.Fatalf("second pop = (%v, %v), want (slot of 2, true)", slot, ok)
	}
	if got := c.size(); got != 0 {
		t.Errorf("size after draining = %d, want 0", got)
	}
}

// TestCompanionBuffer_AppendAcrossSlotBoundaries feeds byte runs that do not
// line up with slot boundaries, mirroring a writer whose frames are not exactly
// one slot each.
func TestCompanionBuffer_AppendAcrossSlotBoundaries(t *testing.T) {
	const slotBytes = 4
	c := newCompanionBuffer(slotBytes, 2)

	// 12 bytes = 3 slots, fed in 5-, 5- and 2-byte runs; bound is 2 slots.
	c.append([]byte{1, 1, 1, 1, 2})
	c.append([]byte{2, 2, 2, 3, 3})
	c.append([]byte{3, 3})

	if got, want := c.size(), 2*slotBytes; got != want {
		t.Fatalf("size = %d, want %d", got, want)
	}
	slot, ok := c.pop()
	if !ok || !bytes.Equal(slot, slotOf(2, slotBytes)) {
		t.Fatalf("first pop = (%v, %v), want (slot of 2, true) — oldest slot should have been dropped", slot, ok)
	}
	slot, ok = c.pop()
	if !ok || !bytes.Equal(slot, slotOf(3, slotBytes)) {
		t.Fatalf("second pop = (%v, %v), want (slot of 3, true)", slot, ok)
	}
}
