#!/usr/bin/env bash
# Emulates the link Skyhook exists for: ~1.2 s RTT, 250 kbps, 2% loss.
#
# Every milestone is tested against this rather than against a real flight;
# real flights are the victory lap. Applies netem to the loopback interface, so
# both the server and the client see the delay (600 ms each way = 1.2 s RTT).
#
# Usage:
#   sudo scripts/netem.sh up            [rtt_ms] [rate_kbit] [loss_pct]
#   sudo scripts/netem.sh port <port>   [rtt_ms] [rate_kbit] [loss_pct]
#   sudo scripts/netem.sh outage        [seconds]
#   sudo scripts/netem.sh down
#   scripts/netem.sh status
#
# "port" is what the test suite uses: it shapes only the traffic to and from the
# Skyhook listener, leaving the CDP socket and the fixture web server alone.
# Those are landside-local in reality and shaping them would emulate the wrong
# thing entirely.
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
  port)
    need_root
    need_tc
    PORT="${2:-}"
    if [ -z "$PORT" ]; then
      echo "usage: $0 port <port> [rtt_ms] [rate_kbit] [loss_pct]" >&2
      exit 2
    fi
    RTT_MS="${3:-1200}"
    RATE_KBIT="${4:-250}"
    LOSS_PCT="${5:-2}"
    HALF_MS=$(( RTT_MS / 2 ))
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    tc qdisc add dev "$IFACE" root handle 1: prio bands 4
    tc qdisc add dev "$IFACE" parent 1:4 handle 40: netem \
      delay "${HALF_MS}ms" 40ms distribution normal \
      loss "${LOSS_PCT}%" \
      rate "${RATE_KBIT}kbit" \
      limit 10000
    for match in dport sport; do
      tc filter add dev "$IFACE" protocol ip parent 1:0 prio 4 u32 \
        match ip "$match" "$PORT" 0xffff flowid 1:4
    done
    echo "netem up on $IFACE for port $PORT: ${RTT_MS}ms RTT, ${RATE_KBIT}kbit, ${LOSS_PCT}% loss"
    ;;
  outage)
    need_root
    need_tc
    SECONDS_DOWN="${2:-60}"
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
    ;;
  *)
    echo "usage: $0 {up|outage|down|status}" >&2
    exit 2
    ;;
esac
