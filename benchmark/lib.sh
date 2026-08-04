# Shared config for the benchmark/ scripts. Source this, don't run it.
#
#   BASE_URL    yasdb's main address, e.g. http://yasdb.internal:4437
#   ADMIN_URL   the /__admin/bulk-provision endpoint on -metrics-addr, e.g.
#               http://yasdb.internal:9091/__admin/bulk-provision (requires
#               the server started with -admin-bulk-provision)
#   STREAMS     stream pool size
#   PAYLOAD_BYTES  append body size
set -euo pipefail

: "${BASE_URL:=http://yasdb.internal:4437}"
: "${ADMIN_URL:=http://yasdb.internal:9091/__admin/bulk-provision}"
: "${STREAMS:=8}"
: "${PAYLOAD_BYTES:=64}"

RUN_ID="$(date +%s%3N)"
PREFIX="/bench/${RUN_ID}/s"

# provision creates $STREAMS empty streams under $PREFIX via bulk-provision:
# one HTTP call, not one PUT per stream (see internal/ds/bulkprovision.go).
provision() {
	curl -sf -X POST "$ADMIN_URL" \
		-H "Content-Type: application/json" \
		-d "{\"pathPrefix\":\"${PREFIX}\",\"count\":${STREAMS},\"contentType\":\"text/plain\"}" \
		>&2
}

# gen_targets writes a vegeta HTTP-format target list cycling through the
# provisioned pool to $1.
gen_targets() {
	python3 -c "
for i in range($STREAMS):
    print(f'POST ${BASE_URL}${PREFIX}{i}')
    print('Content-Type: text/plain')
    print()
" >"$1"
}

# gen_payload writes a $PAYLOAD_BYTES-byte body to $1.
gen_payload() {
	python3 -c "import sys; sys.stdout.write('x' * $PAYLOAD_BYTES)" >"$1"
}
