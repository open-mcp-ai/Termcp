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

echo
printf '\033[1m== %d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then printf '  failed: %s\n' "${FAILED_NAMES[*]}"; fi
exit "$FAIL"
