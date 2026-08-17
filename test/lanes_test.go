package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
		{"a single port, as SKYHOOK_TEST_PORT always meant", "21123", []int{21123}},
		{"a range, inclusive of both ends", "21123-21126", []int{21123, 21124, 21125, 21126}},
		{"a range of one", "21123-21123", []int{21123}},
		{"a list", "21123,21125", []int{21123, 21125}},
		{"ranges and ports mixed", "21123-21124,21130", []int{21123, 21124, 21130}},
		{"overlap collapses, so a lane is never leased twice over",
			"21123-21125,21124-21126", []int{21123, 21124, 21125, 21126}},
		{"spaces and empty entries are ignored", " 21123 - 21125 , , 21130 ",
			[]int{21123, 21124, 21125, 21130}},
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
		"21123-",
		"-21123",
		"0",
		"70000",
		"21126-21123",
		"21123-abc",
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
	lanes <- 21123
	lanes <- 21124

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

/*
A lane the kernel also hands out is refused before the suite starts.

Leasing settles which test may use a port and tells the kernel nothing. Ports
inside ip_local_port_range are handed to whatever asks for "any port" next, and
this suite asks for one per fixture server, per CDN, per app listener, per
browser debugging port and per outbound connection — so a lane in that range is
a lane that gets taken, and what comes back is `bind: address already in use`
on a port the pool believed was free.

That happened to three tests on one netem run, all on the same lane, and read
as three unrelated broken features. Cheaper to refuse the pool.
*/
func TestLanesInsideTheEphemeralRangeAreRefused(t *testing.T) {
	t.Parallel()
	lo, hi, ok := localPortRange()
	if !ok {
		t.Skip("no ip_local_port_range to read; not Linux")
	}

	// One in the middle of the range, which is where the lanes used to sit.
	inside := lo + (hi-lo)/2
	err := lanesAreOursToKeep([]int{inside})
	if err == nil {
		t.Errorf("a lane at %d, inside the kernel's own %d-%d, was accepted", inside, lo, hi)
	}
	// The message has to name the port and say where to put it instead: this
	// fires on a machine whose owner has never heard of ip_local_port_range.
	if err != nil && !strings.Contains(err.Error(), strconv.Itoa(inside)) {
		t.Errorf("the refusal does not name the offending lane: %v", err)
	}

	if lo <= 1 {
		return // nothing below the range to test with
	}
	if err := lanesAreOursToKeep([]int{lo - 1}); err != nil {
		t.Errorf("a lane at %d, below the range, was refused: %v", lo-1, err)
	}
}

// localPortRange reads what the kernel will assign on its own, or reports that
// it could not be read.
func localPortRange() (lo, hi int, ok bool) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &lo, &hi); err != nil {
		return 0, 0, false
	}
	return lo, hi, lo < hi
}
