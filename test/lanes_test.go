package e2e

import (
	"sync"
	"testing"
)

// The shaped-port pool is the one piece of the parallel suite that has no
// browser in it, so unlike everything else in this package it can be tested
// directly. What it has to get right is small and worth pinning down: a spec
// that turns into the wrong ports shapes nothing, and a lease that hands the
// same port to two tests puts them on one 250 kbit link — neither of which
// fails loudly. They just make the suite slow again, or flaky.

func TestParseLanesReadsAPortSpec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec string
		want []int
	}{
		{"empty is no pool at all", "", nil},
		{"a single port, as SKYHOOK_TEST_PORT always meant", "45123", []int{45123}},
		{"a range, inclusive of both ends", "45123-45126", []int{45123, 45124, 45125, 45126}},
		{"a range of one", "45123-45123", []int{45123}},
		{"a list", "45123,45125", []int{45123, 45125}},
		{"ranges and ports mixed", "45123-45124,45130", []int{45123, 45124, 45130}},
		{"overlap collapses, so a lane is never leased twice over",
			"45123-45125,45124-45126", []int{45123, 45124, 45125, 45126}},
		{"spaces and empty entries are ignored", " 45123 - 45125 , , 45130 ",
			[]int{45123, 45124, 45125, 45130}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLanes(tc.spec)
			if err != nil {
				t.Fatalf("parseLanes(%q): %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseLanes(%q) = %v, want %v", tc.spec, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseLanes(%q) = %v, want %v", tc.spec, got, tc.want)
				}
			}
		})
	}
}

func TestParseLanesRejectsWhatItCannotShape(t *testing.T) {
	t.Parallel()
	// A bad spec has to be an error rather than an empty pool: an empty pool
	// silently falls back to ephemeral ports, and the suite would then run
	// unshaped and pass in a fraction of the time, which reads as a triumph.
	for _, spec := range []string{
		"not-a-port",
		"45123-",
		"-45123",
		"0",
		"70000",
		"45126-45123",
		"45123-abc",
	} {
		if got, err := parseLanes(spec); err == nil {
			t.Errorf("parseLanes(%q) = %v, want an error", spec, got)
		}
	}
}

func TestALaneIsLeasedToOneTestAtATime(t *testing.T) {
	t.Parallel()
	// Two lanes, four tests: the pool is what bounds concurrency, so two of
	// them have to wait rather than double up on a port. Anything a test holds
	// must be back in the pool by the time the next one is handed a lane.
	lanes := make(chan int, 2)
	lanes <- 45123
	lanes <- 45124

	var mu sync.Mutex
	held := map[string]string{} // addr -> the test holding it

	for _, name := range []string{"one", "two", "three", "four"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr := leaseFrom(t, lanes)

			mu.Lock()
			if other, taken := held[addr]; taken {
				mu.Unlock()
				t.Fatalf("%s leased %s while %s still held it", name, addr, other)
			}
			held[addr] = name
			mu.Unlock()

			t.Cleanup(func() {
				mu.Lock()
				delete(held, addr)
				mu.Unlock()
			})
		})
	}
}

func TestNoPoolMeansAnEphemeralPort(t *testing.T) {
	t.Parallel()
	// Unshaped runs — `make test-e2e`, or anyone without tc — have no pool and
	// must not block waiting for a lane that is never coming.
	if got := leaseFrom(t, nil); got != "127.0.0.1:0" {
		t.Errorf("leaseFrom(nil) = %q, want an ephemeral address", got)
	}
}
