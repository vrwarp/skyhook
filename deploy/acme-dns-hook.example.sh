#!/bin/sh
# A dns-01 challenge hook for Skyhook, written against Cloudflare's API.
#
# Copy it, change the middle, point the server at it:
#
#   "acme": {
#     "enabled": true, "agreeTos": true, "challenge": "dns-01",
#     "dns": { "command": ["/usr/local/bin/skyhook-dns-hook"] }
#   }
#
# Skyhook runs it twice per record, and passes the same three facts as
# arguments and in the environment — use whichever suits:
#
#   $1 / $SKYHOOK_ACME_ACTION   present | cleanup
#   $2 / $SKYHOOK_ACME_FQDN     _acme-challenge.skyhook.example.com
#   $3 / $SKYHOOK_ACME_VALUE    the TXT value to publish
#
# Exit non-zero to fail the challenge; whatever you print is quoted back in the
# server's error, so print the provider's own message rather than "failed".
#
# TWO THINGS TO GET RIGHT, both of which fail in ways that look like something
# else:
#
#   * `present` must ADD a record, never replace what is at that name. One
#     certificate covering a host and a wildcard over it needs two values at
#     the same name, and overwriting makes the first challenge fail in a way
#     indistinguishable from slow propagation.
#   * `cleanup` must delete only the value it was given, for the same reason,
#     and must succeed when there is nothing to delete — it runs after failures
#     too.
#
# The token is read from the environment, not written here: this file ends up
# in a config directory and a repository, and the token is a credential for
# your whole zone. Give it Zone.DNS edit on one zone and nothing else.
#
#   systemd:  Environment=CF_API_TOKEN=...   (or EnvironmentFile=)
#   docker:   -e CF_API_TOKEN=...
set -eu

ACTION="${1:-$SKYHOOK_ACME_ACTION}"
FQDN="${2:-$SKYHOOK_ACME_FQDN}"
VALUE="${3:-$SKYHOOK_ACME_VALUE}"

: "${CF_API_TOKEN:?set CF_API_TOKEN to a Cloudflare token with Zone.DNS edit}"
API="https://api.cloudflare.com/client/v4"
AUTH="Authorization: Bearer $CF_API_TOKEN"
JSON="Content-Type: application/json"

# The zone is the registered domain, which is the last two labels for a great
# many names and is wrong for the rest (co.uk, com.au, and anything delegated a
# level down). Set CF_ZONE explicitly if this guess does not fit yours.
ZONE="${CF_ZONE:-$(echo "$FQDN" | awk -F. '{print $(NF-1)"."$NF}')}"

api() {
  # -sS: quiet, but still print the error. --fail-with-body would hide the
  # message Cloudflare sends with a 4xx, which is the useful half.
  curl -sS -H "$AUTH" -H "$JSON" "$@"
}

zone_id=$(api "$API/zones?name=$ZONE" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$zone_id" ] || { echo "no Cloudflare zone for $ZONE (set CF_ZONE?)"; exit 1; }

case "$ACTION" in
  present)
    # POST adds; it does not replace. That is the behaviour required above, and
    # it is why this is not a PUT.
    out=$(api -X POST "$API/zones/$zone_id/dns_records" \
      --data "{\"type\":\"TXT\",\"name\":\"$FQDN\",\"content\":\"$VALUE\",\"ttl\":60}")
    echo "$out" | grep -q '"success":true' || { echo "$out"; exit 1; }
    ;;
  cleanup)
    # Only the record carrying this exact value, and quietly if it is gone.
    ids=$(api "$API/zones/$zone_id/dns_records?type=TXT&name=$FQDN&content=$VALUE" \
      | tr ',' '\n' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    for id in $ids; do
      api -X DELETE "$API/zones/$zone_id/dns_records/$id" > /dev/null
    done
    ;;
  *)
    echo "unknown action: $ACTION"; exit 2
    ;;
esac
