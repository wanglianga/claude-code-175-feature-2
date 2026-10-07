#!/usr/bin/env bash
# e2e 公共函数库。
GREEN='\033[0;32m'; RED='\033[0;31m'; YEL='\033[0;33m'; NC='\033[0m'
PASS=0; FAIL=0
BASE="${BASE:-http://localhost:8080}"
# 固定演示 token(服务启动 seed 写入 Redis)
T="tenant-token"; L="landlord-token"; A="agent-token"; P="platform-token"

ok()   { echo -e "${GREEN}  PASS${NC} $1"; PASS=$((PASS+1)); }
bad()  { echo -e "${RED}  FAIL${NC} $1"; FAIL=$((FAIL+1)); }
info() { echo -e "${YEL}== $1 ==${NC}"; }

assert_eq() {
  if [ "$2" == "$3" ]; then ok "$1 = $3"; else bad "$1 期望[$2] 实际[$3]"; fi
}

REQ_CODE=""
REQ_BODY=""
req() {
  local method="$1" path="$2" token="$3" body="${4:-}"
  local tmp; tmp=$(mktemp)
  if [ -n "$body" ]; then
    REQ_CODE=$(curl -s -o "$tmp" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d "$body")
  else
    REQ_CODE=$(curl -s -o "$tmp" -w '%{http_code}' -X "$method" "$BASE$path" \
      -H "Authorization: Bearer $token")
  fi
  REQ_BODY=$(cat "$tmp"); rm -f "$tmp"
}

jqr() { python3 -c "
import sys,json
d=json.load(sys.stdin)
expr=sys.argv[1]
cur=d
for k in expr.split('.'):
    if k=='': continue
    cur=cur.get(k) if isinstance(cur,dict) else cur[int(k)]
print('' if cur is None else cur)
" "$1" 2>/dev/null; }

# 数据库断言连接(可用 PGHOST/PGPORT/PGUSER/PGDATABASE 覆盖, 适配 compose 与本地两种环境)
: "${PGHOST:=127.0.0.1}"
: "${PGPORT:=5433}"
: "${PGUSER:=app}"
: "${PGDATABASE:=deposit}"
PSQL() { psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -tAc "$1"; }

SKIP=0
skip() { echo -e "\033[2m  SKIP\033[0m $1"; SKIP=$((SKIP+1)); }

# kafka 控制台消费者: 优先 $KAFKA_HOME, 其次常见本地路径, 都没有则为空(调用方应 skip)
kafka_consumer_bin() {
  if [ -n "${KAFKA_HOME:-}" ] && [ -x "$KAFKA_HOME/bin/kafka-console-consumer.sh" ]; then
    echo "$KAFKA_HOME/bin/kafka-console-consumer.sh"
  elif [ -x "$HOME/kafka_2.13-3.7.2/bin/kafka-console-consumer.sh" ]; then
    echo "$HOME/kafka_2.13-3.7.2/bin/kafka-console-consumer.sh"
  else
    command -v kafka-console-consumer.sh 2>/dev/null
  fi
}
