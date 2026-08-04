#!/usr/bin/env bash
# smoke — tiny end-to-end validation. Run this first to confirm the target
# server agrees with the protocol before launching a real benchmark. One
# full PUT -> POST -> GET -> HEAD -> DELETE round trip; no load tool needed.
#
#   BASE_URL=https://your-app.fly.dev ./benchmark/smoke.sh
set -euo pipefail

: "${BASE_URL:=http://yasdb.internal:4437}"
PATH_="/bench/smoke/$(date +%s%3N)"
URL="${BASE_URL}${PATH_}"
FAIL=0

check() {
	local name="$1" want="$2" got="$3"
	if [ "$got" = "$want" ]; then
		echo "ok   $name ($got)"
	else
		echo "FAIL $name: want $want, got $got"
		FAIL=1
	fi
}

status=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$URL" -H 'Content-Type: text/plain')
check PUT 201 "$status"

status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL" -H 'Content-Type: text/plain' -d hello)
check POST 204 "$status"

body=$(curl -sf "${URL}?offset=-1")
if [ "$body" = "hello" ]; then
	echo "ok   GET body ($body)"
else
	echo "FAIL GET body: want hello, got $body"
	FAIL=1
fi

status=$(curl -s -o /dev/null -w '%{http_code}' -I "$URL")
check HEAD 200 "$status"

status=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$URL")
check DELETE 204 "$status"

exit $FAIL
