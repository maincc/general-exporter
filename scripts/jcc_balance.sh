#!/bin/sh
# JCC 链上账户余额查询脚本
# 用于 general-exporter custom collector
# 输出格式: 指标名 数值

RPC_URL="${JCC_RPC_URL:-https://srje115qd43qw2.jccdex.cn}"
ACCOUNT="${JCC_ACCOUNT:-jHuNdJbWjpTbZPoHMqrGesWRCw2qUedAdV}"

RESPONSE=$(curl -s -X POST "$RPC_URL" \
  -H 'Content-Type: application/json' \
  -d "{\"method\":\"account_info\",\"params\":[{\"account\":\"${ACCOUNT}\",\"ledger_index\":\"validated\"}]}" \
  --connect-timeout 10 \
  --max-time 15)

# 检查 status
STATUS=$(echo "$RESPONSE" | jq -r '.result.status // empty' 2>/dev/null)
if [ "$STATUS" != "success" ]; then
  echo "jcc_balance_up 0"
  exit 0
fi

BALANCE=$(echo "$RESPONSE" | jq -r '.result.account_data.Balance // empty' 2>/dev/null)
SEQUENCE=$(echo "$RESPONSE" | jq -r '.result.account_data.Sequence // 0' 2>/dev/null)
LEDGER=$(echo "$RESPONSE" | jq -r '.result.ledger_index // 0' 2>/dev/null)

if [ -z "$BALANCE" ]; then
  echo "jcc_balance_up 0"
  exit 0
fi

echo "jcc_balance_up 1"
echo "swtc_balance $BALANCE"
# echo "jcc_sequence $SEQUENCE"
# echo "jcc_ledger_index $LEDGER"
