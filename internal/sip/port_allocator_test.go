package sip

import (
	"fmt"
	"sync"
	"testing"
)

func TestNewPortAllocator_BothZero(t *testing.T) {
	pa, err := NewPortAllocator(0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pa != nil {
		t.Fatal("expected nil allocator when both min and max are 0")
	}
}

func TestNewPortAllocator_OnlyMinSet(t *testing.T) {
	_, err := NewPortAllocator(10000, 0)
	if err == nil {
		t.Fatal("expected error when only min is set")
	}
}

func TestNewPortAllocator_OnlyMaxSet(t *testing.T) {
	_, err := NewPortAllocator(0, 20000)
	if err == nil {
		t.Fatal("expected error when only max is set")
	}
}

func TestNewPortAllocator_MinGreaterThanMax(t *testing.T) {
	_, err := NewPortAllocator(20000, 10000)
	if err == nil {
		t.Fatal("expected error when min >= max")
	}
}

func TestNewPortAllocator_MinEqualsMax(t *testing.T) {
	_, err := NewPortAllocator(10000, 10000)
	if err == nil {
		t.Fatal("expected error when min == max")
	}
}

func TestNewPortAllocator_RangeTooSmall(t *testing.T) {
	_, err := NewPortAllocator(10000, 10050)
	if err == nil {
		t.Fatal("expected error when range < 100")
	}
}

func TestNewPortAllocator_ValidRange(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pa == nil {
		t.Fatal("expected non-nil allocator")
	}
	min, max := pa.Range()
	if min != 10000 || max != 10100 {
		t.Fatalf("expected range 10000-10100, got %d-%d", min, max)
	}
}

func TestAllocate_ReturnsPortsInRange(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 0; i < 10; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		if port < 10000 || port > 10100 {
			t.Fatalf("port %d out of range [10000, 10100]", port)
		}
	}
}

func TestAllocate_NoDuplicates(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seen := make(map[int]bool)
	for i := 0; i < 50; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		if seen[port] {
			t.Fatalf("duplicate port %d on allocation %d", port, i)
		}
		seen[port] = true
	}
}

func TestAllocate_Exhaustion(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Allocate all 101 ports (10000..10100 inclusive)
	for i := 0; i <= 100; i++ {
		_, err := pa.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed unexpectedly: %v", i, err)
		}
	}

	// Next allocation should fail
	_, err = pa.Allocate()
	if err == nil {
		t.Fatal("expected error on exhausted pool")
	}
}

func TestRelease_MakesPortAvailable(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Allocate all ports
	ports := make([]int, 0, 101)
	for i := 0; i <= 100; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		ports = append(ports, port)
	}

	// Release one port
	released := ports[50]
	pa.Release(released)

	// Should be able to allocate again
	port, err := pa.Allocate()
	if err != nil {
		t.Fatalf("allocation after release failed: %v", err)
	}
	if port != released {
		t.Fatalf("expected released port %d, got %d", released, port)
	}

	// Should be exhausted again
	_, err = pa.Allocate()
	if err == nil {
		t.Fatal("expected error after re-exhaustion")
	}
}

func TestAllocate_WrapsAround(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Allocate 90 ports, release first 50, allocate 50 more
	// This forces the cursor to wrap around.
	first := make([]int, 0, 90)
	for i := 0; i < 90; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		first = append(first, port)
	}

	for i := 0; i < 50; i++ {
		pa.Release(first[i])
	}

	for i := 0; i < 50; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("post-release allocation %d failed: %v", i, err)
		}
		if port < 10000 || port > 10100 {
			t.Fatalf("port %d out of range after wrap", port)
		}
	}
}

func TestAllocate_Concurrent(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const goroutines = 20
	const perGoroutine = 5

	var mu sync.Mutex
	allPorts := make(map[int]bool)
	errs := make(chan error, goroutines*perGoroutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				port, err := pa.Allocate()
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				if allPorts[port] {
					mu.Unlock()
					errs <- fmt.Errorf("duplicate port %d", port)
					return
				}
				allPorts[port] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent error: %v", err)
	}

	if len(allPorts) != goroutines*perGoroutine {
		t.Fatalf("expected %d unique ports, got %d", goroutines*perGoroutine, len(allPorts))
	}
}

func TestRelease_ConcurrentWithAllocate(t *testing.T) {
	pa, err := NewPortAllocator(10000, 10100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Pre-allocate 80 ports
	ports := make([]int, 0, 80)
	for i := 0; i < 80; i++ {
		port, err := pa.Allocate()
		if err != nil {
			t.Fatalf("setup allocation %d failed: %v", i, err)
		}
		ports = append(ports, port)
	}

	// Concurrently release and allocate
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for _, p := range ports[:40] {
			pa.Release(p)
		}
	}()

	go func() {
		defer wg.Done()
		// Try allocating; some may fail before releases happen, that's OK
		for i := 0; i < 40; i++ {
			pa.Allocate()
		}
	}()

	wg.Wait()
}
