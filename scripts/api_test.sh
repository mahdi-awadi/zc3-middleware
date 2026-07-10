#!/usr/bin/env bash
# Full live API test pass against the running middleware.
# Usage: BASE=http://localhost:8080 scripts/api_test.sh
set -u
BASE="${BASE:-http://localhost:8080}"
KEY="${API_KEY:-}"
AUTH=(); [ -n "$KEY" ] && AUTH=(-H "Authorization: Bearer $KEY")
pass=0; fail=0

check() { # name method path expected_code [body]
  local name="$1" method="$2" path="$3" want="$4" body="${5:-}"
  local args=(-s -o /tmp/apiout -w "%{http_code}" -X "$method" "${AUTH[@]}" "$BASE$path")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  local code; code=$(curl "${args[@]}")
  if [ "$code" = "$want" ]; then
    printf "  PASS  %-28s %s %s -> %s\n" "$name" "$method" "$path" "$code"; pass=$((pass+1))
  else
    printf "  FAIL  %-28s %s %s -> %s (want %s)\n" "$name" "$method" "$path" "$code" "$want"; fail=$((fail+1))
  fi
}

echo "== zc3-middleware live API test =="
echo "base: $BASE"
echo
echo "-- health / device --"
check healthz          GET  /healthz                200
check readyz           GET  /readyz                 200
check device           GET  /device                 200
check params           GET  "/params?keys=~SerialNumber,IPAddress,LockCount" 200
echo "-- data tables (read) --"
check users            GET  /users                  200
check cards            GET  /cards                  200
check tables           GET  /tables                 200
check table_timezone   GET  /tables/timezone        200
check table_user       GET  /tables/user            200
check table_userauth   GET  /tables/userauthorize   200
check table_txn        GET  /tables/transaction     200
echo "-- live status / events / metrics --"
check status           GET  /status                 200
check history          GET  "/events/history?limit=5" 200
check metrics          GET  /metrics                200
echo "-- write gating (ALLOW_WRITES off => 502) --"
check enroll_gated     POST /cards                  502 '{"card_no":"1","doors":[1]}'
echo "-- validation --"
check bad_door         POST /doors/x/open           400
echo
echo "== $pass passed, $fail failed =="
exit $fail
