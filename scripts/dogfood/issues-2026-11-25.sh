#!/usr/bin/env bash
# Dogfood loop for the 2026-11-25 issue batch (#327-#356).
#
# It drives a REAL termcp instance over the same two entrances an agent uses —
# POST /api/term (the DSL) and POST /api/... (the REST surface) — and checks the
# acceptance criteria of each issue. No unit tests: the point of a dogfood round is
# that the product, assembled and running, answers the way the issue asked.
#
#   scripts/dogfood/issues-2026-11-25.sh [--port N] [--keep]
#
# Exit code is the number of failed checks (0 = all green). The instance is built
# from the working tree, started on a spare port with a throwaway data dir, and
# stopped on exit unless --keep is given.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

PORT=${PORT:-18791}
KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/termcp-dogfood.XXXXXX")
BIN="$WORK/termcp"
DATA="$WORK/data"
mkdir -p "$DATA"
LOG="$WORK/termcp.log"

PASS=0
FAIL=0
FAILED_NAMES=()

cleanup() {
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
    kill -TERM "$PID" 2>/dev/null
    for _ in $(seq 1 40); do kill -0 "$PID" 2>/dev/null || break; sleep 0.05; done
    kill -KILL "$PID" 2>/dev/null
  fi
  if [ "$KEEP" = "1" ]; then
    echo "kept: $WORK (log: $LOG)"
  else
    mv "$WORK" "${TMPDIR:-/tmp}/termcp-dogfood-done.$$" 2>/dev/null || true
  fi
}
trap cleanup EXIT

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); FAILED_NAMES+=("$*"); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }

# check NAME CONDITION_OUTPUT EXPECTED_SUBSTRING — pass when EXPECTED appears in OUTPUT.
check() {
  local name="$1" got="$2" want="$3"
  if printf '%s' "$got" | grep -qF -- "$want"; then ok "$name"; else
    bad "$name"
    printf '       want substring: %s\n' "$want"
    printf '       got: %s\n' "$(printf '%s' "$got" | head -c 800 | tr '\n' '|')"
  fi
}

# check_not NAME OUTPUT UNWANTED_SUBSTRING — pass when UNWANTED does not appear.
check_not() {
  local name="$1" got="$2" unwanted="$3"
  if printf '%s' "$got" | grep -qF -- "$unwanted"; then
    bad "$name"
    printf '       unwanted substring present: %s\n' "$unwanted"
    printf '       got: %s\n' "$(printf '%s' "$got" | head -c 800 | tr '\n' '|')"
  else ok "$name"; fi
}

build() {
  say "build"
  if ! go build -o "$BIN" . 2>"$WORK/build.err"; then
    echo "build failed:"; cat "$WORK/build.err"; exit 1
  fi
  echo "  built $BIN"
}

start() {
  say "start on :$PORT"
  "$BIN" --data-dir "$DATA" --port "$PORT" --log-level info >"$LOG" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 200); do
    if curl -sf "http://127.0.0.1:$PORT/api/forwards" >/dev/null 2>&1; then echo "  ready (pid $PID)"; return 0; fi
    kill -0 "$PID" 2>/dev/null || { echo "  died; log:"; tail -30 "$LOG"; exit 1; }
    sleep 0.1
  done
  echo "  never became ready; log:"; tail -30 "$LOG"; exit 1
}

# term CMD — POST a term command line, print the reply text.
term() { curl -s -X POST "http://127.0.0.1:$PORT/api/term" -H 'Content-Type: application/json' \
  --data-binary "$(python3 -c 'import json,sys; print(json.dumps({"cmd": sys.argv[1]}))' "$1")"; }
# termj JSON — POST a raw term argument object.
termj() { curl -s -X POST "http://127.0.0.1:$PORT/api/term" -H 'Content-Type: application/json' --data-binary "$1"; }
# body JSON — print the reply's body field, decoded (so the assertions can quote
# the prose a caller actually reads rather than its JSON escaping).
body() { printf '%s' "$1" | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("body",""))
except Exception: print("")'; }
# api METHOD PATH [BODY] — print "CODE<TAB>BODY".
api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -o "$WORK/api.out" -w '%{http_code}' -X "$method" "http://127.0.0.1:$PORT$path" \
      -H 'Content-Type: application/json' --data-binary "$body"
  else
    curl -s -o "$WORK/api.out" -w '%{http_code}' -X "$method" "http://127.0.0.1:$PORT$path"
  fi
  printf '\t'; cat "$WORK/api.out"
}

# new_session — create an internal pty session, print its id.
new_session() {
  api POST /api/sessions '{"ssh_config":"internal","mode":"pty"}' |
    python3 -c 'import sys,json; raw=sys.stdin.read(); code,body=raw.split("\t",1); print(json.loads(body)["session_id"])'
}

build
start

SID=$(new_session)
if [ -z "$SID" ]; then echo "could not create a session; log:"; tail -40 "$LOG"; exit 1; fi
echo "  session $SID"

# ---------------------------------------------------------------------------
say "#338 term 接受 command 作为 cmd 的别名"
out=$(termj '{"command":"ls"}')
check "#338 command 单独出现时执行" "$out" '"code":200'
check "#338 别名在回复里可见" "$out" '"Hint"'
check "#338 列表结果照常返回" "$out" '"body":"'"$SID"
out=$(termj '{"cmd":"ls","command":"ls"}')
check "#338 两者相同则放行" "$out" '"code":200'
out=$(termj '{"cmd":"ls","command":"ls host"}')
check "#338 两者不同则拒绝" "$out" '"code":400'
check "#338 冲突消息点名两个字段" "$out" 'cmd and command'
out=$(termj '{"command":123}')
check "#338 非字符串拒绝" "$out" '"code":400' 

# ---------------------------------------------------------------------------
say "#352 `|` 只是 read|write，不是通用管道"
out=$(term "grep x | head")
b=$(body "$out")
check "#352 拒绝 shell 管道（左侧不是指令也一样）" "$out" '"code":400'
check "#352 报错点名 read | write 这一对" "$b" 'read | write is the only pairing'
check "#352 报错说明不是通用管道" "$b" 'not a general pipe'
check "#352 报错指出 shell 管道写在 payload 里" "$b" 'run <channel> $1'
check_not "#352 不再把 | 说成通用管道操作符" "$b" 'only pipeline this language has'
out=$(term "read $SID:1 | grep x")
b=$(body "$out")
check "#352 右侧不是指令时也点名 pair" "$out" '"code":400'
check "#352 且给出 payload 写法" "$b" 'ls | wc -l'
out=$(term "read $SID:1 | write $SID file /tmp/pipe-target.txt")
check "#352 read | write 仍然可用" "$out" '202'
out=$(term "read $SID:1 | write $SID file /a | write $SID file /b")
check "#352 三端串联被拒绝而不是猜顺序" "$out" '"code":400'

# The downstream half of #352: an embedder that inherits this package without a copy
# verb must not be told about read | write. Its own vocabulary decides the sentence.
# The program has to live INSIDE this module to import termcp/term, so it is written
# into a scratch dir at the repo root and moved out of the tree as soon as it has run.
EMB_DIR=".dogfood-embedder.$$"
mkdir -p "$EMB_DIR"
cat > "$EMB_DIR/main.go" <<'GO'
package main

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/term"
)

func main() {
	// A language with no pipe ends at all — what zero-memcp has.
	// Flags verbs: a payload verb keeps the `|` as part of its payload, so the
	// operator reading only applies to verbs that take neither a payload nor a
	// RestValue flag — which is exactly where a discarded right side is a bug.
	g := term.NewGrammar([]term.Op{
		{Name: "create", Kind: term.KindFlags},
		{Name: "list", Kind: term.KindFlags},
	})
	_, err := term.New(g).Parse("create x | list", term.Subst{})
	fmt.Println(strings.ReplaceAll(fmt.Sprint(err), "\n", " | "))
}
GO
emb=$(go run "./$EMB_DIR/main.go" 2>&1)
mv "$EMB_DIR" "$WORK/embedder-src"
check "#352 下游无 pipe 的词汇不说 read | write" "$emb" "no verb here pipes into another"
check_not "#352 下游报错里不出现 read" "$emb" "read | write is the only"

# ---------------------------------------------------------------------------
# #356 needs a real MCP peer. This instance serves /stream itself, so the peer is a
# second mount of the same server: no network, no fixture, and the tool table is real.
say "#356 mcp tools 地址与可复制示例"
out=$(term "open $SID mcp http://127.0.0.1:$PORT/stream")
check "#356 挂一个 MCP peer" "$out" '"code":200'
check "#356 挂载回复给出 mcp:1 句柄" "$out" 'mcp:1'
out=$(term "get $SID mcp:1 tools")
check "#356 带编号的地址可列工具" "$out" '"code":200'
out=$(term "get $SID mcp tools")
check "#356 文档教的 mcp tools（不带编号）现在也通" "$out" '"code":200'
check "#356 两种写法给同一张表" "$out" "tool(s) on $SID mcp:1"
out=$(term "get $SID mcp:1")
b=$(body "$out")
check "#356 get 到 peer 本身不 panic" "$out" '"code":200'
check_not "#356 没有 internal_error" "$out" 'internal_error'
check "#356 并指出下一步是 tools" "$b" 'tools'
out=$(term "get $SID mcp:1 bogus")
check "#356 认不出的词按 400 拒绝" "$out" '"code":400'
check "#356 拒绝时点名 tools" "$out" 'tools'
check_not "#356 不再静默返回描述" "$out" '"code":200'
# The body is read decoded: the JSON escapes `<` and `>` as \u003c/\u003e, and a
# check on the raw reply would be testing the escaper rather than the text.
out=$(term "help get"); b=$(body "$out")
check "#356 help get 用真地址形态" "$b" 'mcp:1 tools'
check_not "#356 help get 不再教不带编号的 mcp tools" "$b" 'a1f3 mcp tools'
out=$(term "help call"); b=$(body "$out")
check "#356 help call 用真地址形态" "$b" 'mcp:1 tools'
out=$(term "help mcp"); b=$(body "$out")
check "#356 help mcp 用真地址形态" "$b" 'mcp:<n> tools'
check "#356 并说明编号可以省略的条件" "$b" 'exactly one MCP peer'
out=$(term "ls $SID mcp")
check "#356 ls mcp 印出可复制的句柄" "$out" "$SID mcp:1"

# `ls ext` 的 next 行：装一个扩展并打开，表里就该给出可复制的调用。
out=$(term "create ext self --target internal --url http://127.0.0.1:$PORT/stream")
check "#356 创建一个扩展" "$out" '"code":200'
out=$(term "open ext self")
check "#356 打开扩展" "$out" '"code":200'
out=$(term "ls ext"); b=$(body "$out")
check "#356 挂载后的行给一行 next 调用" "$b" '→ get'
check "#356 next 行用真地址而不是占位符" "$b" 'mcp:1 tools'
check "#356 表尾一行 next" "$b" 'next: get'
check_not "#356 占位符 <peer> 不再出现" "$b" '<peer>'
# 未挂载的行不给 next：那一行只有名字本身，重复四十遍会把有地址的行埋掉。
out=$(term "create ext idle-one --target internal --url http://127.0.0.1:$PORT/stream")
check "#356 再建一个扩展" "$out" '"code":200'
out=$(term "ls ext"); b=$(body "$out")
check_not "#356 未挂载的行不重复打印 open ext" "$b" '→ open ext idle-one'
check "#356 已挂载的行仍然有 next" "$b" '→ get' 

# ---------------------------------------------------------------------------
say "#341 close 批量"
A=$(new_session); B=$(new_session); C=$(new_session)
out=$(term "open $A --argv sh -c 'sleep 60'"); check "#341 准备第二个通道" "$out" '"code":200'
out=$(term "open $A --argv sh -c 'sleep 60'"); check "#341 准备第三个通道" "$out" '"code":200'
out=$(term "close $A:2,$A:3")
check "#341 一次关两个通道" "$out" '"code":204'
check "#341 每个槽点出自己关的是谁" "$out" "\"Batch\":\"$A:2\""
check "#341 第二个槽也一样" "$out" "\"Batch\":\"$A:3\""
check_not "#341 仍然活着" "$out" 'running'
out=$(term "close $A:1,nosuch:9")
check "#341 失败的那条给出 404" "$out" '"code":404'
check "#341 成功的那条不被回滚" "$out" '"code":204'
out=$(term "close $B --index 1")
check "#341 --index 指出单个通道" "$out" "$B:1"
out=$(term "close $B:1 --all")
check "#341 --all 落在通道上被拒绝" "$out" '"code":400'
check "#341 并给出正确写法" "$out" "close $B"
out=$(term "close $C --all")
check "#341 --all 落在会话上就是关整个会话" "$out" '"code":204'
out=$(term "close $C:1 --index 1")
check "#341 --index 落在通道上被拒绝" "$out" '"code":400'

say "#339 rm 批量与归档清空"
D=$(new_session); E=$(new_session)
out=$(term "close $D"); check "#339 先结束一个会话" "$out" '"code":204'
out=$(term "rm $D,$E")
check "#339 死的那条被清" "$out" '"code":204'
check "#339 活的那条被拒绝" "$out" 'session_running'
check "#339 两条各自成槽" "$out" "\"Batch\":\"$E\""
out=$(term "rm --dead --dry-run")
check "#339 --dead --dry-run 只预告" "$out" 'Dry-Run'
check "#339 预告里没有已清的 D" "$out" '"code":200'
out=$(term "rm --dead --force")
check "#339 --force 不能用在 --dead 上" "$out" '"code":400'
check "#339 并说明 --dead 不动活会话" "$out" 'never touches a live session'
out=$(term "rm --dead")
check "#339 一次清空归档" "$out" '"code":200'
check "#339 清空归档不影响活会话" "$out" '0 failed'
# 会话活着时 `ls` 印的是它的通道地址（`<session>:1`），不是裸 id，所以按前缀断言。
out=$(term "ls"); b=$(body "$out")
check "#339 运行中的会话还在" "$b" "$E:1"
check_not "#339 已清的 D 不在列表里" "$b" "$D"

# ---------------------------------------------------------------------------
say "#340 read/write 支持目录"
TREE="$WORK/tree"
mkdir -p "$TREE/sub"
printf 'hi\n' > "$TREE/a.txt"
printf 'yo\n' > "$TREE/sub/b.txt"
mkdir -p "$TREE/deep/er"
out=$(term "read $SID file $TREE"); b=$(body "$out")
check "#340 读目录不再报 is a directory" "$out" '"code":200'
check_not "#340 也不再是二进制报错" "$b" 'is a directory'
check "#340 列出条目名" "$b" 'a.txt'
check "#340 标出目录" "$b" 'sub/'
check "#340 表里带大小" "$b" '3'
check "#340 表里带类型标记" "$b" 'd  sub/'
out=$(term "read $SID file $TREE --depth 2"); b=$(body "$out")
check "#340 --depth 走下去（深层行印全路径）" "$b" 'sub/b.txt'
out=$(term "read $SID file $TREE --limit 2"); b=$(body "$out")
check "#340 --limit 封顶" "$out" '"code":200'
check "#340 并说清被截掉多少" "$b" 'more not listed'
check "#340 深度封顶时说清还有几层没走" "$b" 'directories not walked'
out=$(term "read $SID file $TREE/a.txt"); b=$(body "$out")
check "#340 文件照旧按字节读" "$b" 'hi'
out=$(term "write $SID file $TREE --data hi"); b=$(body "$out")
check "#340 写目录默认被拒绝" "$out" '"code":400'
check "#340 拒绝语给出批量写法" "$b" '--recursive'
check "#340 并给出多语句写法" "$b" 'write <session> file'
out=$(term "write $SID file $TREE --recursive --dry-run --data '{\"x.txt\":\"hi\",\"sub/y.txt\":\"yo\"}'")
check "#340 dry-run 只预告" "$(body "$out")" 'would write'
check_not "#340 dry-run 没有真的写" "$(ls $TREE)" 'x.txt'
out=$(term "write $SID file $TREE --recursive --data '{\"x.txt\":\"hi\",\"sub/y.txt\":\"yo\"}'"); b=$(body "$out")
check "#340 批量写逐条回报" "$b" 'ok    '
check "#340 写法条数" "$b" 'wrote 2 file(s)'
check "#340 子目录被建出来" "$(cat $TREE/sub/y.txt 2>/dev/null)" 'yo'
out=$(term "write $SID file $TREE --recursive --data '{\"/etc/passwd\":\"x\"}'")
check "#340 绝对路径被拒绝" "$out" '"code":400'
out=$(term "write $SID file $TREE --recursive --data '{\"../x\":\"x\"}'")
check "#340 越界路径被拒绝" "$out" '"code":400'
check "#340 越界拒绝说明原因" "$out" 'climbs out'
out=$(term "write $SID file $TREE --recursive --data 'not json'")
check "#340 非对象 payload 被拒绝" "$out" '"code":400'

# ---------------------------------------------------------------------------
say "#354 call 的 frame 表达力"
printf '{"jsonrpc":"2.0","id":1,"method":"ping"}' > "$WORK/frame.json"
out=$(term "call $SID mcp:1 --file $WORK/frame.json")
check "#354 --file 从文件发 frame" "$out" '"code":200'
check "#354 frame 真的发出去了" "$(body "$out")" '"id":1'
out=$(term "call $SID mcp:1 @$WORK/frame.json")
check "#354 @path 是 --file 的短写法" "$out" '"code":200'
out=$(term "call $SID mcp:1 --file $WORK/frame.json '{\"a\":1}'")
check "#354 --file 与内联 frame 同时给被拒绝" "$out" '"code":400'
check "#354 并说明该丢哪个" "$out" 'two different frames'
out=$(term "call $SID mcp:1 --file -")
check "#354 --file - 被拒绝并说明原因" "$out" '"code":400'
check "#354 指出 stdin 是协议通道" "$out" 'standard input'
out=$(term "call $SID mcp:1 --file $WORK/nope.json")
check "#354 文件不存在时点名路径" "$out" '"code":400'
check "#354 并说明路径在哪台机器上" "$out" 'running termcp'
# 撇号：POSIX 拼接在 verbatim payload 里被还原成一个引号。
out=$(term "call $SID mcp:1 '{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"ping\",\"params\":{\"n\":\"it'\\''s\"}}'")
check "#354 单引号里的撇号（拼接写法）能发出去" "$out" '"code":200'
# 写错的转义要指向真正的那一层，并给出三种可行写法。
out=$(term "call $SID mcp:1 '{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"ping\",\"params\":{\"n\":\"it\\'s\"}}'")
check "#354 反斜杠撇号被拒绝" "$out" '"code":400'
check "#354 报错说明 payload 是 verbatim" "$out" 'VERBATIM payload'
check "#354 并给出 --file 的出路" "$out" '--file /tmp/frame.json'

# ---------------------------------------------------------------------------
# #330：via 让一条隧道建在别的连接上。本机没有第二台可 SSH 的主机，所以这里验的是
# 规则与失败隔离——两条都是"能不能安全地写"的部分；真机上的双向验证在 issue 里
# 需要一台 VPS，报告里说明。
say "#330 forward via"
FWD_VIA='[{"kind":"remote","via":"nosuchhost","local_port":18921,"remote_host":"127.0.0.1","remote_port":'"$PORT"'}]'
out=$(term "create ext viabad --target internal --url http://127.0.0.1:$PORT/stream --forwards '$FWD_VIA'")
check "#330 带 via 的模板可以保存" "$out" '"code":200'
check "#330 模板里保留了 via" "$(body "$out")" '"Via": "nosuchhost"'
sleep 3
out=$(term "get ext viabad"); b=$(body "$out")
check "#330 隧道失败不影响扩展可用" "$b" 'status: available'
check "#330 失败点名是哪台机器" "$b" '"via": "nosuchhost"'
check "#330 失败只记在 forward 这一段" "$b" '"stage": "forward"'
check "#330 并排了重试时间" "$b" 'next_retry:'
# endpoint 在隧道坏着的时候照常可用（failures 是隧道的事，不是通道的事）。
out=$(term "open ext viabad")
check "#330 隧道坏着也能挂上 MCP peer" "$out" '"code":200'
HANDLE=$(printf '%s' "$out" | python3 -c 'import sys,json,re
b=json.load(sys.stdin).get("body","")
m=re.search(r"session_id: (\S+)", b)
print(m.group(1) + " mcp:1" if m else "")')
out=$(term "get $HANDLE tools")
check "#330 隧道坏着 endpoint 仍然回答问题" "$out" '"code":200'
# 规则：via 不能是本扩展自己的连接 / 不能用在 dynamic / 不能写在 [[targets]] 下
out=$(term "create ext bad1 --target internal --url http://127.0.0.1:$PORT/stream --forwards '[{\"kind\":\"remote\",\"via\":\"internal\",\"local_port\":18922,\"remote_host\":\"127.0.0.1\",\"remote_port\":$PORT}]'")
check "#330 via 指自己会被拒绝" "$out" '"code":400'
check "#330 并说明省略 via 就是它" "$out" 'own connection'
out=$(term "create ext bad2 --target internal --forwards '[{\"kind\":\"dynamic\",\"via\":\"x\",\"local_port\":18923}]'")
check "#330 dynamic 上的 via 被拒绝" "$out" '"code":400'
out=$(term "create ext bad3 --targets '[{\"host\":\"internal\",\"forwards\":[{\"kind\":\"remote\",\"via\":\"x\",\"local_port\":18924,\"remote_host\":\"127.0.0.1\",\"remote_port\":22}]}]'")
check "#330 [[targets]] 下的 via 被拒绝" "$out" '"code":400'
check "#330 并说明它已经属于那个 host" "$out" 'already'

# ---------------------------------------------------------------------------
# #331：模板 v2 —— 一个 ext 多条通道，每条各自的连接与句柄。
say "#331 template v2 ([[channel]])"
cat > "$WORK/two.toml" <<TOML
name = "twocard"
target = "internal"
resident = true
description = "an endpoint and a worker from one card"

[[channel]]
name = "api"
kind = "http"
url = "http://127.0.0.1:$PORT/stream"

[[channel]]
name = "worker"
kind = "stdio"
command = "cat"
TOML
out=$(term "create ext twocard --template @$WORK/two.toml")
b=$(body "$out")
check "#331 --template @file 装得进去" "$out" '"code":200'
check "#331 两条通道都起来了" "$b" '"name": "api"'
check "#331 第二条也有状态" "$b" '"kind": "stdio"'
sleep 3
b=$(body "$(term "get ext twocard")")
check "#331 api 拿到了句柄" "$b" 'api'
check "#331 两条都 available" "$b" 'available'
check "#331 worker 有自己的 shell" "$b" 'worker'
# 一条卡片两条通道 → 同一个会话里两个进程，第二进不去的地方就是 bug。
SID=$(printf '%s' "$b" | python3 -c 'import sys,re
m=re.search(r"session_id: (\S+)", sys.stdin.read())
print(m.group(1) if m else "")')
b=$(body "$(term "ls $SID")")
check "#331 api 的通道在会话里" "$b" 'twocard.api'
check "#331 worker 的通道并排存在" "$b" 'twocard.worker'
# 寻址：--channel 指名哪条；名字不存在要说清有哪些。
b=$(body "$(term "open ext twocard --channel worker")")
check "#331 --channel 取到 worker" "$b" 'channel: worker'
out=$(term "open ext twocard --channel nope")
check "#331 不存在的通道是 400" "$out" '"code":400'
check "#331 并列出合法名字" "$out" 'api, worker'
# 旧模板一个通道，形状不变：没有 channels 这一节。
term "create ext singlecard --target internal --mode pipe --command cat" >/dev/null
sleep 2
b=$(body "$(term "get ext singlecard")")
check_not "#331 单通道模板不出现 channels" "$b" 'channels:'
check "#331 单通道仍然是懒的（形状没变）" "$b" 'status: idle'
# 两种说法不能混写：顶层 command 与 [[channel]] 二选一，按名字拒绝而不是合并。
cat > "$WORK/mixed.toml" <<TOML
name = "mixed"
target = "internal"
command = "cat"
mode = "pipe"

[[channel]]
name = "worker"
kind = "stdio"
command = "cat"
TOML
out=$(term "create ext mixed --template @$WORK/mixed.toml")
check "#331 顶层 command 与 [[channel]] 混写被拒" "$out" '"code":400'
check "#331 并说明二选一" "$out" 'alternatives'
# 名字是寻址：重名就没有唯一答案。
cat > "$WORK/dup.toml" <<TOML
name = "dup"
target = "internal"

[[channel]]
name = "worker"
kind = "stdio"
command = "cat"

[[channel]]
name = "worker"
kind = "stdio"
command = "cat"
TOML
out=$(term "create ext dup --template @$WORK/dup.toml")
check "#331 重名通道被拒" "$out" '"code":400'
check "#331 并说明名字要唯一" "$out" 'twice'
# 一个端点不接受 command：那是两条通道。
cat > "$WORK/httpcmd.toml" <<TOML
name = "httpcmd"
target = "internal"

[[channel]]
name = "api"
kind = "http"
url = "http://127.0.0.1:$PORT/stream"
command = "cat"
TOML
out=$(term "create ext httpcmd --template @$WORK/httpcmd.toml")
check "#331 http 通道带 command 被拒" "$out" '"code":400'
check "#331 并说明那要写成两条通道" "$out" 'two channels'

# ---------------------------------------------------------------------------
# #328：/ext 的传输接缝。#328 的四步里这一步（S1+S2）是「不再自己写一份传输」：
# session id、GET 流、状态码、body 上限都来自共用实现，端点只剩「把一帧写成一行、
# 等回答」。
say "#328 /ext 走共用传输"
term "create ext echo --target internal --mode pipe --command cat" >/dev/null
sleep 2
EXT="http://127.0.0.1:$PORT/ext/echo/stream"
# GET 缺 session 是 400 并点名头（此前是 405 + 一段解释，而客户端恰恰在这里等推送）。
out=$(curl -s -o /dev/null -w '%{http_code}' -X GET "$EXT")
check "#328 GET 缺 session 是 400" "$out" '400'
out=$(curl -s -X GET "$EXT")
check "#328 并点名缺哪个头" "$out" 'Mcp-Session-Id'
# initialize 拿到 session id：此前这个端点没有 id，后面的 GET 无从指名。
hdr=$(mktemp)
out=$(curl -s -D "$hdr" -X POST "$EXT" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}')
check "#328 initialize 有回答" "$out" '"jsonrpc":"2.0"'
check "#328 并给出 Mcp-Session-Id" "$(cat "$hdr")" 'Mcp-Session-Id:'
SID_EXT=$(grep -i '^mcp-session-id:' "$hdr" | tr -d '\r' | awk '{print $2}')
# 通知是 202（规范如此）；请求不许 202——那条差别正是旧实现丢掉的一半。
out=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$EXT" -H 'Content-Type: application/json' \
  -H "Mcp-Session-Id: $SID_EXT" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}')
check "#328 通知回 202" "$out" '202'
out=$(curl -s -X POST "$EXT" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $SID_EXT" \
  -d '{"jsonrpc":"2.0","id":2,"method":"ping"}')
check "#328 请求拿到的是帧，不是 202" "$out" '"id":2'
# 批量帧按名字拒绝：2025-06-18 没有 batch。
out=$(curl -s -X POST "$EXT" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $SID_EXT" \
  -d '[{"jsonrpc":"2.0","id":3,"method":"ping"}]')
check "#328 批量帧被拒绝" "$out" 'batch'
# DELETE 结束 session（旧实现是假成功，什么都不做）。
out=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$EXT" -H "Mcp-Session-Id: $SID_EXT")
check "#328 DELETE 结束 session" "$out" '204'
out=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$EXT" -H 'Content-Type: application/json' \
  -H "Mcp-Session-Id: $SID_EXT" -d '{"jsonrpc":"2.0","id":4,"method":"ping"}')
check "#328 结束后的 session 是 404" "$out" '404'

# ---------------------------------------------------------------------------
# #328 S3：常驻 pump 与四路分帧。用一个真对话的桩 MCP server，因为它会**自己说话**
# （notifications/acme/job），而 cat 只会回声——回声验不出「没人请求时帧能不能到」。
say "#328 pump：没人 POST 时通知也能到"
cat > "$WORK/stub_mcp.py" <<'PYEOF'
import sys, json, threading, time
def out(o):
    sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()
def push():
    time.sleep(0.2)
    n = 0
    while True:
        n += 1
        out({"jsonrpc": "2.0", "method": "notifications/acme/job", "params": {"seq": n}})
        time.sleep(0.3)
threading.Thread(target=push, daemon=True).start()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        m = json.loads(line)
    except Exception:
        continue
    if "id" not in m:
        continue
    if (m.get("params") or {}).get("slow"):
        time.sleep(float(m["params"]["slow"]))
    out({"jsonrpc": "2.0", "id": m["id"], "result": {
        "serverInfo": {"name": "stub", "version": "1"},
        "protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
        "echoed": m.get("params", {})}})
PYEOF
term "create ext stub --target internal --mode pipe --command python3 --args $WORK/stub_mcp.py" >/dev/null
sleep 2
EXT2="http://127.0.0.1:$PORT/ext/stub/stream"
# 开一个 session（initialize），后面所有请求都用它。
HSESS="$WORK/sess.hdr"
curl -s -D "$HSESS" -o /dev/null -X POST "$EXT2" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
S1=$(grep -i '^mcp-session-id:' "$HSESS" | tr -d '\r' | awk '{print $2}')
# The session id is random; what matters is that the handshake produced one at all.
check_not "#328 桩扩展确实起来了（拿到了 session）" "$S1" 'unknown'
# 1) 没有任何 POST 在进行时，GET 流在 1s 内收到扩展自己发出的通知。
timeout 2 curl -s -N -X GET "$EXT2" -H "Mcp-Session-Id: $S1" > "$WORK/stream.out"
check "#328 GET 流收到扩展主动发的帧" "$(cat "$WORK/stream.out")" 'notifications/acme/job'
# 2) 两个客户端都用 id:1，各拿各的回答。
HSESS2="$WORK/sess2.hdr"
curl -s -D "$HSESS2" -o /dev/null -X POST "$EXT2" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
S2=$(grep -i '^mcp-session-id:' "$HSESS2" | tr -d '\r' | awk '{print $2}')
curl -s -X POST "$EXT2" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $S1" \
  -d '{"jsonrpc":"2.0","id":1,"method":"ping","params":{"who":"A"}}' > "$WORK/a.out" &
PA=$!
curl -s -X POST "$EXT2" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $S2" \
  -d '{"jsonrpc":"2.0","id":1,"method":"ping","params":{"who":"B"}}' > "$WORK/b.out" &
PB=$!
# BARE `wait` WOULD WAIT FOR THE DAEMON TOO — it is a background job of this shell, so the
# script would hang until the suite timer fired. Wait for the two curls by pid.
wait "$PA" "$PB"
check "#328 客户端 A 拿到自己的回答" "$(cat "$WORK/a.out")" '"who": "A"'
check "#328 客户端 B 拿到自己的回答" "$(cat "$WORK/b.out")" '"who": "B"'
check_not "#328 两个 id:1 不串包（A 没拿到 B 的）" "$(cat "$WORK/a.out")" '"who": "B"'
# 3) initialize 只转给对端一次：第二个客户端拿到的是缓存，回包里的 serverInfo 仍是扩展自己的。
out=$(curl -s -X POST "$EXT2" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}')
check "#328 第二个 initialize 复用缓存" "$out" '"serverInfo"'
check "#328 且 id 改回调用方的" "$out" '"id": 42'
# 3) 一个慢调用：窗口不是工具超时，客户端等到的是结果（旧实现会先回 202）。
started=$(date +%s)
out=$(curl -s -X POST "$EXT2" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $S1" \
  -d '{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"slow":2,"who":"slow"}}')
elapsed=$(( $(date +%s) - started ))
check "#328 慢调用等到了结果" "$out" '"who": "slow"'
check "#328 确实等了（不是立刻 202）" "$elapsed" '2'

# 6) 扩展在请求中途消失：客户端拿到可读的错误，而不是挂到窗口用完。
started=$(date +%s)
curl -s -X POST "$EXT2" -H 'Content-Type: application/json' -H "Mcp-Session-Id: $S1" \
  -d '{"jsonrpc":"2.0","id":88,"method":"tools/call","params":{"slow":30}}' > "$WORK/teardown.out" &
PT=$!
sleep 2
term "rm ext stub" >/dev/null
wait "$PT"
elapsed=$(( $(date +%s) - started ))
check "#328 扩展消失后请求拿到错误" "$(cat "$WORK/teardown.out")" '"error"'
check "#328 并说明通道没了" "$(cat "$WORK/teardown.out")" 'channel ended'
if [ "$elapsed" -lt 10 ]; then fast=yes; else fast="no(${elapsed}s)"; fi
check "#328 而且很快（没有挂到窗口用完）" "$fast" 'yes'

# ---------------------------------------------------------------------------
# #310 Phase 3.1 — 被动 OSC 133：装了 shell integration 的机器，普通 run 也能拿到退出码
#
# 这里自己造标记（本机 zsh 没装 integration），验的是「读不读」而不是「shell 发不发」：
# 标记进了 PTY 字节流，termcp 就该像 `--until exit` 那样把它读出来。
echo
echo "== #310 被动 OSC 133"
H310=$(body "$(term "connect internal --name osc310")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
check "#310 起了通道" "$H310" ':'

# 1) 不写 --until，标记照样被读出来（此前只认 --until exit）。
R310=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"run "+sys.argv[1]+" $1","args":["printf \u0027OUT\\n\\033]133;D;3\\a\u0027"],"timeout":3}))' "$H310")")
check "#310 普通 run 拿到 Completion" "$R310" '"Completion":"exit"'
check "#310 并带上了退出码" "$R310" '"Exit-Code":3'

# 2) 没有标记就不许凭空说完成：quiet 不是证据。
R310b=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"run "+sys.argv[1]+" $1","args":["echo plain310"],"timeout":2}))' "$H310")")
check_not "#310 没有标记就不报 Completion" "$R310b" '"Completion"'

# 3) 搜索过的 run 与普通 run 一样听得见（两条读路径都接上了）。
R310c=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"run "+sys.argv[1]+" --grep OUT $1","args":["printf \u0027OUT\\n\\033]133;D;9\\a\u0027"],"timeout":3}))' "$H310")")
check "#310 --grep 路径也拿到退出码" "$R310c" '"Exit-Code":9'

# 4) 早先命令的标记不算这一次的：窗口从 Input-At 起算。
R310d=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"run "+sys.argv[1]+" $1","args":["echo later310"],"timeout":2}))' "$H310")")
check_not "#310 早先的标记不算这一次" "$R310d" '"Completion"'

term "close $H310" >/dev/null

# ---------------------------------------------------------------------------
# #357 §1.3 — secrets 的值不出现在回复回显里，但服务端拿到的仍是真值
echo
echo "== #357 secrets 不回显"
H357=$(body "$(term "connect internal --name sec357")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
check "#357 起了通道" "$H357" ':'

W357=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"write "+sys.argv[1]+" file /tmp/dog357.txt --data $SV","secrets":{"SV":"DOGFOOD-SECRET-357"},"timeout":3}))' "$H357")")
check_not "#357 回显里没有 secret 的值" "$W357" 'DOGFOOD-SECRET-357'
check "#357 回显里留着引用名（可再抄一次）" "$W357" '$SV'
check "#357 写入本身成功" "$W357" 'ok: true'
# 防自证：值从文件读回来，证明服务端拿到的是真值而不是被打码后的字符串。
R357=$(body "$(term "read $H357 file /tmp/dog357.txt")")
check "#357 落盘的是真值" "$R357" 'DOGFOOD-SECRET-357'
# 对照：vars 是记录的一部分，仍原样回显。
V357=$(termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"write "+sys.argv[1]+" file /tmp/dog357b.txt --data $VV","vars":{"VV":"DOGFOOD-PLAIN-357"},"timeout":3}))' "$H357")")
check "#357 vars 仍原样回显（它是记录）" "$V357" 'DOGFOOD-PLAIN-357'
SID357="${H357%%:*}"; term "rm $SID357 file /tmp/dog357.txt; rm $SID357 file /tmp/dog357b.txt" >/dev/null
term "close $H357" >/dev/null

# ---------------------------------------------------------------------------
# #357 §1.2 — 精确替换的回执要说清改了哪一行，而不是只报字节数
echo
echo "== #357 编辑可见性"
H357E=$(body "$(term "connect internal --name edit357")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
check "#357 起了通道" "$H357E" ':'
S357E="${H357E%%:*}"
termj "$(python3 -c 'import json,sys
print(json.dumps({"cmd":"write "+sys.argv[1]+" file /tmp/edit357.txt --data $D","vars":{"D":"port = 80\ntimeout = 5\nretries = 3\n"},"timeout":3}))' "$H357E")" >/dev/null
E357=$(body "$(term "write $H357E file /tmp/edit357.txt --old-text 'timeout = 5' --new-text 'timeout = 30'")")
check "#357 回执给出行号" "$E357" 'line: 2'
check "#357 回执给出改动前后" "$E357" '- timeout = 5 | + timeout = 30'
check "#357 仍然报字节数" "$E357" 'bytes_written'
term "rm $S357E file /tmp/edit357.txt" >/dev/null
term "close $H357E" >/dev/null

# ---------------------------------------------------------------------------
# #316 — pipe 模式的 stdio MCP 扩展，按 HTTP peer 的方式调用工具
echo
echo "== #316 扩展 stdio 通道即 MCP peer"
cat > "$WORK/stub316.py" <<'PYEOF'
import sys, json
def out(o):
    sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    mid, method = req.get("id"), req.get("method")
    if mid is None:
        continue
    if method == "initialize":
        out({"jsonrpc": "2.0", "id": mid, "result": {"protocolVersion": "2025-06-18",
             "capabilities": {"tools": {}}, "serverInfo": {"name": "stub316", "version": "1"}}})
    elif method == "tools/list":
        out({"jsonrpc": "2.0", "id": mid, "result": {"tools": [
            {"name": "ping316", "description": "returns pong316", "inputSchema": {"type": "object"}},
            {"name": "fail316", "description": "reports failure", "inputSchema": {"type": "object"}}]}})
    elif method == "tools/call":
        name = (req.get("params") or {}).get("name")
        if name == "ping316":
            out({"jsonrpc": "2.0", "id": mid, "result": {"content": [{"type": "text", "text": "pong316"}]}})
        elif name == "fail316":
            out({"jsonrpc": "2.0", "id": mid, "result": {"content": [{"type": "text", "text": "boom316"}], "isError": True}})
        else:
            out({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no tool " + str(name)}})
    else:
        out({"jsonrpc": "2.0", "id": mid, "result": {}})
PYEOF
term "create ext stub316 --target internal --command python3 --args $WORK/stub316.py" >/dev/null
# THE HANDLE IS THE EXTENSION'S OWN shell_id, NOT `session:1`: extensions that dial the
# same host with the same capability set SHARE one connection, so index 1 may belong to a
# neighbour — and the handshake would then be written to somebody else's process.
body "$(term "open ext stub316")" >/dev/null
H316=$(body "$(term "get ext stub316")" | grep -oE 'shell_id: [0-9a-f]+' | awk '{print $2}')
L316=$(body "$(term "get $H316 tools")")
check "#316 get <channel> tools 列出工具表" "$L316" 'ping316'
check "#316 工具表教的是本地址的调用形状" "$L316" "call $H316 <tool>"
C316=$(body "$(term "call $H316 ping316 {}")")
check "#316 call <channel> <tool> 返回工具文本" "$C316" 'pong316'
check_not "#316 返回的不是信封" "$C316" '"jsonrpc"'
F316=$(body "$(term "call $H316 fail316 {}")")
check "#316 isError 报成失败而不是成功" "$(term "call $H316 fail316 {}")" '"code":500'
check "#316 失败保留工具自己的话" "$F316" 'boom316'
check "#316 peer 的 error 对象指名消息" "$(body "$(term "call $H316 nosuch {}")")" 'no tool nosuch'
check "#316 --json 仍可取原始信封" "$(body "$(term "call $H316 --json ping316 {}")")" '"jsonrpc"'
P316=$(body "$(term "connect internal --argv sh")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
check "#316 普通 pipe 仍只收裸帧" "$(term "call $P316 ping316 {}")" 'the frame is not JSON'
term "close $P316" >/dev/null
term "close $H316" >/dev/null

# ---------------------------------------------------------------------------
# #186 的适配半边 — 反斜杠的盘符路径被改写，并且回执说出来
echo
echo "== #186 反斜杠路径适配"
H186=$(body "$(term "connect internal --name spell186")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
A186=$(body "$(term "read $H186 file 'C:\\tc\\a.txt'")")
check "#186 回执说出改了拼写" "$A186" 'C:\tc\a.txt -> /C:/tc/a.txt'
# 白名单按同一份规则比较：声明 /C:/tc 就应当放行反斜杠写法（否则是 403 而不是 404）。
term "create ext sp186 --target internal --paths /C:/tc" >/dev/null
body "$(term "open ext sp186")" >/dev/null
SID186=$(body "$(term "get ext sp186")" | grep -oE 'session_id: [0-9a-f]+' | awk '{print $2}')
W186=$(term "read $SID186 file 'C:\\tc\\a.txt'")
check_not "#186 声明过的树不会因拼写被拒" "$W186" 'TERM/1 403'
check "#186 同一次调用也说出适配" "$W186" '/C:/tc/a.txt'
# 逃生口：名字里真带反斜杠的 POSIX 文件，原样可达。
printf 'LITERAL-BACKSLASH\n' > "$WORK/C:\tc.txt"
LIT186=$(body "$(term "read $H186 file '$WORK/C:\\tc.txt'")")
check "#186 名字含反斜杠的文件仍可读" "$LIT186" 'LITERAL-BACKSLASH'
check_not "#186 且没有被改写" "$LIT186" 'Path-Adapted'
term "close $H186" >/dev/null
term "rm ext sp186" >/dev/null

# ---------------------------------------------------------------------------
# #325 — get ext 的对账字段：liveness 是测量，不是状态字
echo
echo "== #325 ext 对账字段"
printf '#!/bin/sh\nexit 7\n' > "$WORK/die325.sh"; chmod +x "$WORK/die325.sh"
term "create ext die325 --target internal --command $WORK/die325.sh" >/dev/null
term "open ext die325" >/dev/null
sleep 1.5
D325=$(body "$(term "get ext die325")")
check "#325 死掉的通道报 alive=false" "$D325" '"alive": false'
check "#325 死掉的通道说出原因（不是"从未有进程"）" "$D325" 'has closed (exit code 7)'
check "#325 不谎报 pid" "$D325" 'pid_unknown'
H325=$(body "$(term "connect internal --name live325")" | grep -oE '[0-9a-f]{12}:[0-9]+' | head -1)
term "create ext live325b --target internal --command sh --args /dev/stdin" >/dev/null
body "$(term "open ext live325b")" >/dev/null
L325=$(body "$(term "get ext live325b")")
check "#325 活着的通道报 alive=true" "$L325" '"alive": true'
check "#325 带探测时间戳" "$L325" 'checked_at'
check "#325 带最后活跃时间" "$L325" 'last_active_at'
term "close $H325" >/dev/null

echo
printf '\033[1m== %d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then printf '  failed: %s\n' "${FAILED_NAMES[*]}"; fi
exit "$FAIL"
