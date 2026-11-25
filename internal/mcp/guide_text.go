package mcp

import (
	"fmt"
	"strings"
)

// THE HUMAN-FACING MATERIAL (#355): one copy, two access paths — the `guide` tool
// and the read-only resources termcp://guide/<topic> (both in guide.go).
//
// WHY IT IS NOT `help`. `help` answers "what can this command language do", which is
// the machine's question and the wrong one to hand a person: it is a directive table,
// it assumes the reader already knows that a host comes before a session, and it has
// no way to say "an empty ls ext is normal". This file answers the person's question —
// what do I configure next, and which of these steps is mine rather than the agent's —
// in prose an assistant can read out loud, with commands copied from the same usage
// lines the parser enforces so that no example is invented here.
//
// WHY IT IS CAPPED. A page here is pasted into a conversation when a person asks "so
// what now?", and a manual is not an answer. The first-run page is held under
// guideBudgetBytes (guide.go measures it and logs a defect), so it covers the first
// five minutes and lists the topics; depth is one topic call away.
//
// WHAT STAYS OUT. Not the directive table, not the HTTP API (that is <origin>/api.md,
// a transport reference), not the extension template fields (`help create` is the
// authority on those). What is here is the order of operations and the traps that are
// invisible from the command language alone.

// guideStartID is the first-run page: the tool's answer with no topic, and
// termcp://guide/start. It is not a topic — topics are the pages reached from it.
const guideStartID = "start"

// guideTopic is one page of the material.
type guideTopic struct {
	// ID is what the tool's topic argument takes, and the last segment of the URI.
	ID string
	// Summary is one line saying what the page gives you. The first-run page's index
	// is rendered from these, and so is each resource's description, so the list of
	// topics cannot disagree with the pages that exist.
	Summary string
	// Text is the page itself: prose a person reads, commands as they are written.
	Text string
}

// guideTopics is the whole material, in the order the index lists it.
var guideTopics = []guideTopic{
	{
		ID:      "connect",
		Summary: "reach a machine: a live session, or one command with no session",
		Text: `Reaching a machine

One tree, three words: a host is saved configuration, a session is one live SSH
link, a channel is one shell inside it. You type into a channel and you read back
from a channel.

TWO WAYS IN, AND THE CHOICE CHANGES WHAT YOU GET

  connect mybox        a live link. The reply is a channel address (a1f3:1) to
                       type into now, read now, and read again later.
  run use:mybox $1     one command over the pool: its output and its exit code,
                       no session, and nothing left to look at afterwards.

Connect when the job is interactive, long, or has output worth reading twice.
Use run use:<host> when it is one non-interactive command.

EVERY CALL IS A NEW LINK. Two connect calls give two sessions; nothing is reused
behind your back, so a reply always describes the link it just made.

WHICH KIND OF CHANNEL

  pipe (--argv)   no echo, and the process's own exit code is readable: scripts,
                  batch work, "did it actually succeed".
  pty             a terminal: prompts, password entry, full-screen programs. It
                  reports bytes rather than completion, so its reply is a sample
                  of the screen — read again to see more.

  connect mybox --argv htop       a pipe channel already running that program
  open a1f3 --argv make           add a pipe channel to a live session
  open a1f3 --pty --argv vim      add a terminal channel

TYPE, SUBMIT AND READ ARE THREE SEPARATE ACTIONS

  run a1f3:1 $1                   type the line, press Enter, read the answer
  input a1f3:1 $1                 type without pressing Enter
  key a1f3:1 enter                press it
  read a1f3:1 --tail 40           sample the screen again

Your bytes go to whoever owns the channel's input, so a program waiting for an
answer takes the line. That is what makes a prompt answerable, and it is also why
a line typed into a full-screen program can vanish into it.

PASSWORDS, sudo AND CONFIRMATIONS

They are questions for a person, not parameters. Reach that person with notify,
then wait for their answer on the channel. The assistant cannot answer them, and
having delivered a notification is not the same as having received one.

WHEN IT DOES NOT CONNECT

The reply names the reason: an unknown host (ls host), a refused credential, an
unconfirmed host key. Fix the entry with edit host rather than retrying, and if
the entry is somebody else's to fix, say so instead of guessing.`,
	},
	{
		ID:      "host",
		Summary: "save, change and remove connection targets; why a write is refused",
		Text: `Connection targets

A host is what you save once: a name, an address, a user, and how to
authenticate. It outlives every session, and connecting never changes it.

LOOK

  ls host             every saved target, plus the built-in "internal" entry
  get host mybox      one entry, without connecting

ADD

  create host mybox --host 10.0.0.5 --user you --private-key "<key>"
  create host staging --source mybox --name staging      copy an existing entry

Further fields, all optional: --port, --description, --default-shell,
--default-mode, --dial-timeout-seconds, --proxy, and --jump-host with --jump-user
/ --jump-port for a bastion. A password or key written here is write-only: no
listing echoes it back.

CHANGE

  edit host mybox --port 2222
  rm host mybox          an entry is deleted with rm; edit never removes one

Editing an entry does not touch sessions already open on it, so connect again to
pick up the change.

WHY A WRITE IS REFUSED WITH 403

Host writes are the operator's switch, not a client setting: an instance started
without --mcp-manage-ssh-configs refuses create, edit and rm, and the refusal names
that flag. Reads are available either way, and nobody but whoever started termcp
can turn it on.

WHERE THE ENTRIES LIVE

One file per entry under the data directory (ssh_configs/<name>/config.toml).
"internal" is a built-in loopback profile rather than a file, and an instance
started with --no-internal hides it.

A SAVED ENTRY IS NOT A WORKING ONE

A profile can be stored and still fail: a wrong user, an address nothing answers
on, a host key nobody has confirmed. Saving proves nothing about reachability, so
connect before telling a person their machine is set up — the SSH message in the
reply is what they need to act on.`,
	},
	{
		ID:      "ext",
		Summary: "install an extension: MCP servers, skills, tunnels, file windows",
		Text: `Extensions: capability you install once

An extension is a pinned, capability-trimmed session that stays warm. It is
configuration plus a process, not a plugin you write code against. Four shapes:

  --command with --mode pipe   a program on a pipe channel (a stdio MCP server)
  --url                        an HTTP MCP endpoint, relayed by termcp
  --paths                      a read-only window onto remote files
  --forwards                   tunnels that come up and go down with the session

LOOK, THEN INSTALL

  ls ext                 what is installed here — an empty list is a normal answer
  get ext boxfs          one template, as stored, with its notes
  create ext boxfs --target internal --paths /var/log
  create ext srv --target internal --mode pipe --command npx --args -y some-server
  open ext boxfs         start it; the reply carries session_id, shell_id and notes

DRIVE WHAT IT OFFERS

  ls ext                                 each extension's address, for example mcp:1
  get <session> mcp:1 tools              the tools it exposes
  call <session> mcp:1 <tool> {"k":"v"}  one tool call: JSON in, text out

The notes inside an extension are its operating manual, and get ext prints them.
Read them before calling its tools: they say what a caller is expected to do first.

A FILE WINDOW IS GATED TWICE, ON PURPOSE

--allow names the operations (file_read, file_stat, and so on) and --paths names
the directories. A path outside that list is refused even when the operation is
allowed, which is what makes a template safe to hand to somebody else.

ONE TEMPLATE CAN CARRY SEVERAL OF THESE

A forwards list beside --command is still one extension: the program runs on the
target, the tunnels come up on that same session, and they live and die together.
With a forward the extension is resident from startup; without one it starts when
first used and is reclaimed after its idle window.

WHEN AN INSTALLED THING NEEDS A REBUILD

  edit ext boxfs --restart

A git pull changes the file, not the session template, so the process keeps running
the code it started with. --restart is the call that notices, and it is refused in
combination with other edits.`,
	},
	{
		ID:      "mcp",
		Summary: "drive another MCP server through termcp",
		Text: `Driving another MCP server through termcp

Two shapes, and the difference is who owns the connection: an extension is
installed once and stays warm (see the ext topic), while a peer is opened on one
live session and goes away with it.

A PEER LIVES ON A SESSION, WHICH IS THE POINT

  open a1f3 mcp http://localhost:8080/mcp     an HTTP endpoint
  open a1f3 mcp ws://localhost:8080/mcp       a WebSocket endpoint
  open a1f3 --argv my-mcp-server              a stdio server, as a channel

termcp does the handshake and discovers the tools as part of open.

THE URL NAMES A NETWORK, NOT A MACHINE

The address is resolved from the session's host. http://localhost:8080 opened
through "prod-box" is prod-box's loopback, reached inside its SSH link, so a
server running there needs no tunnel and no published port.

CALL A TOOL — THE COMMON CASE

  get  a1f3 mcp:1 tools                       what it offers
  get  a1f3 mcp:1 tools search                one tool's input schema
  call a1f3 mcp:1 search {"query":"x"}        call it; the text result is unpacked
  call a1f3 mcp:1 --json search {"query":"x"} keep the raw response envelope

The flags of call go BEFORE the tool name and its arguments, not after them.

THE NUMBER IS PART OF THE ADDRESS

mcp:1 is the connection number. Leaving it out works only while that session has
exactly one MCP peer, and is refused — with the live list — the moment a second one
is mounted. Copy the mcp:N that ls ext or ls <session> prints.

A METHOD THAT IS NOT A TOOL CALL

  call a1f3 mcp:1 '{"jsonrpc":"2.0","id":1,"method":"ping"}'    one raw frame
  read a1f3 mcp:1                        notifications that arrived unsolicited

The frame is the rest of the line, so quote it. A channel opened with --argv is not
a peer: it carries whole frames only, with no handshake and no tool-name form.

BIG RESULTS ARRIVE AS A RECEIPT

When a result exceeds the byte window, termcp answers with the frame number instead
of the body. Page it rather than re-sending the call:

  read a1f3 mcp:1 --at <frame> --offset 0 --bytes 4096

AN EMPTY REPLY IS NOT AN ANSWER

A frame the peer has not sent yet is neither a success nor a failure. Read again, or
with a longer window, before concluding that a tool returned nothing.`,
	},
	{
		ID:      "cdp",
		Summary: "drive a real browser over the DevTools protocol",
		Text: `Driving a real browser

CDP is the browser's own protocol: JSON-RPC over a socket inside Chrome. termcp is
a transport for it and interprets no method, so the command list comes from the
browser itself: get a1f3 cdp:1 protocol, or one domain — get a1f3 cdp:1 protocol Page.

START ONE, THEN OPEN IT

  open internal --argv chrome --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 about:blank
  open internal cdp http://127.0.0.1:9222

--remote-debugging-address=127.0.0.1 keeps the port off every other interface;
without it the operating system asks a human to approve a firewall rule for a
throwaway debug port. An already-running browser is opened the same way, or by its
ws://host:port/devtools/browser/<id> address.

A TAB IS A SESSION

The browser process answers Target.* (getTargets, createTarget, attachToTarget).
Page.*, Runtime.* and DOM.* need the sessionId that attaching returns, and the tab
address fills it in, so the frames do not have to:

  ls a1f3 cdp:1                            the tabs in that connection
  open a1f3 cdp:1 --target <targetId>      attach one — the reply is a1f3 cdp:1@<id>
  call a1f3 cdp:1@<id> '{"method":"Page.enable"}'
  call a1f3 cdp:1@<id> '{"method":"Page.navigate","params":{"url":"https://example.test"}}'
  call a1f3 cdp:1 '{"method":"Target.getTargets"}'      a browser-level method

THE ONE SILENT MISTAKE

A page-level method sent without the tab address goes to the browser process, which
answers "wasn't found". A Page.enable sent before the attach succeeds and delivers
nothing, forever. Enable a domain after the attach, not before it.

TWO NUMBERS, TWO THINGS

cdp:1 is WHICH CONNECTION, so two browsers are cdp:1 and cdp:2; @<id> is WHICH TAB
inside one. One connection manages the whole browser, so there is no need to open a
connection per tab. With two connections open, an unqualified cdp is refused rather
than guessed — driving the wrong browser while the reply said it worked is exactly
the failure that rule prevents.

EVENTS ARE GONE ONCE THEY HAVE FIRED

  read a1f3 cdp:1 --since N     frames after N, without consuming them

An event that fired while nothing was reading is not replayed, so read before the
moment you care about rather than after it.`,
	},
}

// guideStartIntro is the first-run page's prose. The index that follows it is
// rendered from guideTopics, so this text names no topic of its own.
const guideStartIntro = `termcp hands your AI a shell on the machines you already have. It reaches them over
SSH with accounts you configured, and it installs nothing on the far side.

THE FIRST FIVE MINUTES — read this part to the person, then do it

1. Save the machine. No connection is made:
     create host mybox --host 10.0.0.5 --user you --private-key "<key>"
   Use --password "<password>" instead of a key if that is what you have.
2. Check that it landed. ls host lists every saved target, plus the built-in
   "internal" entry which means this machine itself.
3. Open it and run one command:
     connect mybox
     run <the channel it answered> $1        args: ["uname -a"]
   The reply carries the address to type into, for example a1f3:1. Copy it — the
   channel is what every later read and write names.
4. Close it when you are done:  close <session>

IF SAVING A HOST IS REFUSED WITH 403

That switch belongs to whoever started termcp: without --mcp-manage-ssh-configs the
instance refuses host writes and the refusal names that flag. Reading (ls host, get
host mybox) works either way, and no client-side setting can lift it. It is their
decision, so ask them instead of retrying.

IF ls ext ANSWERS "(no extensions installed)"

That is normal on a fresh install, and nothing is broken. An extension is an optional
add-on for work you repeat — another MCP server, a set of skills, a tunnel, a
read-only view of some files. A first task needs none of it.

WHAT THIS IS NOT

Not the command reference: help inside term() is that, every directive and its flags,
generated from the parser that enforces them. Read this to the person; use help
yourself.
`

// guideTopicIDs is the topic list, in page order.
func guideTopicIDs() []string {
	ids := make([]string, 0, len(guideTopics))
	for _, t := range guideTopics {
		ids = append(ids, t.ID)
	}
	return ids
}

// guidePageText renders one page. An empty id is the first-run page, which is also
// reachable as "start": the resource URI says termcp://guide/start, and a caller that
// read that URI should not be told the word it just used does not exist.
//
// The second return value is false for an unknown id, which is the caller's cue to
// answer with the list rather than with an empty page.
func guidePageText(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" || id == guideStartID {
		return guideStartText(), true
	}
	for _, t := range guideTopics {
		if t.ID == id {
			return t.Text, true
		}
	}
	return "", false
}

// guideStartText is the first-run page: its prose, then the topic index rendered
// from the pages that exist. The index is generated rather than written out so that
// adding a topic cannot leave the front page pointing at a page that is gone.
func guideStartText() string {
	var b strings.Builder
	b.WriteString(guideStartIntro)
	b.WriteString("\nONE PAGE PER SUBJECT\n")
	for _, t := range guideTopics {
		fmt.Fprintf(&b, "  %-7s  %s\n", t.ID, t.Summary)
	}
	b.WriteString("\nThe same pages are read-only MCP resources — termcp://guide/start and\n")
	b.WriteString("termcp://guide/<topic> — so reading one costs no tool call.\n")
	return b.String()
}

// guideTopicList is the topic list as one line, for a refusal or a schema.
func guideTopicList() string {
	return strings.Join(guideTopicIDs(), ", ")
}
