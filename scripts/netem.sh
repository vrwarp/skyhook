#!/usr/bin/env bash
# Emulates the link Skyhook exists for: ~1.2 s RTT, 250 kbps, 2% loss.
#
# Every milestone is tested against this rather than against a real flight;
# real flights are the victory lap. Applies netem to the loopback interface, so
# both the server and the client see the delay (600 ms each way = 1.2 s RTT).
#
# Usage:
#   sudo scripts/netem.sh up                    [rtt_ms] [rate_kbit] [loss_pct]
#   sudo scripts/netem.sh port  <port>          [rtt_ms] [rate_kbit] [loss_pct]
#   sudo scripts/netem.sh lanes <base> <count>  [rtt_ms] [rate_kbit] [loss_pct]
#   sudo scripts/netem.sh outage        [seconds]
#   sudo scripts/netem.sh down
#   scripts/netem.sh status
#
# "port" shapes only the traffic to and from the Skyhook listener, leaving the
# CDP socket and the fixture web server alone. Those are landside-local in
# reality and shaping them would emulate the wrong thing entirely.
#
# "lanes" is what the test suite uses, and it is "port" repeated: <count>
# consecutive ports from <base>, each with a netem qdisc of its own. The
# repetition is the point. A netem qdisc's `rate` is a budget for everything
# queued into it, so N tests sharing one shaped port would divide 250 kbit
# between them and finish no sooner than they would have one after another —
# the emulated link, not the CPU, is what the suite spends its time on. A
# qdisc per port gives each concurrent test the whole link, which is what a
# test is supposed to be measuring, and what makes running them at the same
# time worth anything.
#
# `port <p>` and `lanes <p> 1` build the identical qdisc tree.
set -euo pipefail

IFACE="${SKYHOOK_NETEM_IFACE:-lo}"
RTT_MS="${2:-1200}"
RATE_KBIT="${3:-250}"
LOSS_PCT="${4:-2}"

need_tc() {
  if ! command -v tc >/dev/null; then
    echo "tc not found: install iproute2 (apt install iproute2)" >&2
    exit 1
  fi
}

need_root() {
  if [ "$(id -u)" != "0" ]; then
    echo "netem needs root: re-run with sudo" >&2
    exit 1
  fi
}

case "${1:-status}" in
  up)
    need_root
    need_tc
    HALF_MS=$(( RTT_MS / 2 ))
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    # netem's delay is applied per direction; half the RTT each way.
    tc qdisc add dev "$IFACE" root netem \
      delay "${HALF_MS}ms" 40ms distribution normal \
      loss "${LOSS_PCT}%" \
      rate "${RATE_KBIT}kbit" \
      limit 10000
    echo "netem up on $IFACE: ${RTT_MS}ms RTT, ${RATE_KBIT}kbit, ${LOSS_PCT}% loss"
    ;;
  port|lanes)
    need_root
    need_tc
    MODE="$1"
    BASE_PORT="${2:-}"
    if [ "$MODE" = "lanes" ]; then
      COUNT="${3:-4}"
      RTT_MS="${4:-1200}"
      RATE_KBIT="${5:-250}"
      LOSS_PCT="${6:-2}"
    else
      COUNT=1
      RTT_MS="${3:-1200}"
      RATE_KBIT="${4:-250}"
      LOSS_PCT="${5:-2}"
    fi
    if [ -z "$BASE_PORT" ]; then
      echo "usage: $0 port <port> [rtt_ms] [rate_kbit] [loss_pct]" >&2
      echo "       $0 lanes <base> <count> [rtt_ms] [rate_kbit] [loss_pct]" >&2
      exit 2
    fi
    case "$BASE_PORT$COUNT" in
      *[!0-9]*) echo "port and count must be numbers" >&2; exit 2 ;;
    esac
    # prio tops out at 16 bands and the first three are spoken for below, so
    # thirteen lanes is the ceiling. It is far above what a runner can drive:
    # each lane costs a test, and each test costs one or two Chromiums.
    if [ "$COUNT" -lt 1 ] || [ "$COUNT" -gt 13 ]; then
      echo "count must be between 1 and 13 (prio allows 16 bands, 3 are reserved)" >&2
      exit 2
    fi
    HALF_MS=$(( RTT_MS / 2 ))
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    # Bands 1:1 to 1:3 are where the default priomap sends everything that no
    # filter claims, and they keep their default pfifo: the CDP socket and the
    # fixture servers stay landside-fast. Shaped lanes start at 1:4.
    tc qdisc add dev "$IFACE" root handle 1: prio bands $(( COUNT + 3 ))
    i=0
    while [ "$i" -lt "$COUNT" ]; do
      band=$(( i + 4 ))
      lane_port=$(( BASE_PORT + i ))
      # tc parses the minor half of a class ID as hex, while `bands` above is
      # decimal, so band 10 has to be written 1:a. Spelling it 1:10 asks for
      # minor 0x10 and fails with "Specified class not found" — but only from
      # the seventh lane on, because below ten the two spellings agree.
      hband=$(printf '%x' "$band")
      # netem's delay is applied per direction; half the RTT each way.
      tc qdisc add dev "$IFACE" parent "1:${hband}" handle "${hband}0:" netem \
        delay "${HALF_MS}ms" 40ms distribution normal \
        loss "${LOSS_PCT}%" \
        rate "${RATE_KBIT}kbit" \
        limit 10000
      # Filter priority is its own decimal thing, unrelated to the class ID.
      for match in dport sport; do
        tc filter add dev "$IFACE" protocol ip parent 1:0 prio "$band" u32 \
          match ip "$match" "$lane_port" 0xffff flowid "1:${hband}"
      done
      i=$(( i + 1 ))
    done
    if [ "$COUNT" = 1 ]; then
      echo "netem up on $IFACE for port $BASE_PORT: ${RTT_MS}ms RTT, ${RATE_KBIT}kbit, ${LOSS_PCT}% loss"
    else
      echo "netem up on $IFACE for ports ${BASE_PORT}-$(( BASE_PORT + COUNT - 1 )):" \
        "${COUNT} independent lanes of ${RTT_MS}ms RTT, ${RATE_KBIT}kbit, ${LOSS_PCT}% loss"
    fi
    ;;
  outage)
    need_root
    need_tc
    SECONDS_DOWN="${2:-60}"
    # This replaces the root qdisc outright, so it takes any lanes with it and
    # drops every port on the interface rather than the shaped ones. It is an
    # operator's tool for watching one session reconnect, not something to run
    # underneath a suite: re-apply `lanes` afterwards.
    echo "simulating a ${SECONDS_DOWN}s outage on $IFACE"
    tc qdisc replace dev "$IFACE" root netem loss 100%
    sleep "$SECONDS_DOWN"
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    echo "link restored; a session should be usable again within 2s"
    ;;
  down)
    need_root
    need_tc
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    echo "netem removed from $IFACE"
    ;;
  status)
    need_tc
    tc qdisc show dev "$IFACE"
    # Which ports are shaped lives in the filters, not the qdiscs, and with
    # lanes that is the only place the mapping is visible at all.
    tc filter show dev "$IFACE" 2>/dev/null || true
    ;;
  *)
    echo "usage: $0 {up|port|lanes|outage|down|status}" >&2
    exit 2
    ;;
esac
