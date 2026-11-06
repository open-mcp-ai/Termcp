// The jrpc/directive detail pages.
//
// They are separate from mcpDetail on purpose, and they are SHORT. mcpDetail is
// long because the MCP handshake, the two handle kinds and the frame quoting are
// each a trap; `jrpc` has none of those — it moves frames and says so — and a
// caller that reads a page three times the size of the contract learns nothing
// extra. cdpDetail is long for a different reason: the traps there are the
// browser's (which URL to open, how a tab is addressed, that an event is gone
// once it has fired), not the protocol's.
package directive

// jrpcDetail is the page for the door that adds nothing.
const jrpcDetail = `A raw JSON-RPC frame channel. Nothing here interprets a method: what you send is
what the peer receives, and what it answers is what you get back. That is the
whole contract, and it is why any peer that speaks the envelope works — an LSP
server, a private RPC, a device.

  jrpc open <session> <url>  ws:// wss:// http:// https:// — scheme picks transport
                             the session decides WHICH network the URL names
  jrpc call <handle> '<frame>'   send, wait for the answer with that id
  jrpc send <handle> '<frame>'   send, do not wait (a notification takes no answer)
  jrpc recv <handle>             read a frame that arrived while a call was waiting
  jrpc close <handle>

The frame is the REST of the line. Quote it. An id is filled in for you when the
frame has none, so call <handle> '{"method":"x"}' is enough.

EVENTS. Frames that arrive while a call is waiting and do not carry its id are
kept, not dropped: a peer that interleaves events with answers (DevTools does)
does not lose them. recv takes one. On a SOCKET they accumulate between calls; on
HTTP there is nothing between calls, so recv answers 204 and says so.

No handshake, no session header, no method table: those belong to a protocol, and
this is the layer under one. Use mcp for an MCP server and cdp for a browser,
where each door adds the one thing its protocol needs.`

// cdpDetail is the page for the browser door.
//
// The traps it documents are the ones that cost a caller a browser lifecycle to
// find: which URL to open, that a page needs enable-ing before it reports
// anything, that a tab is addressed by a sessionId which does not exist until
// AFTER the attach — so the enable calls must follow the attach, not precede it —
// and that an event which fired while nobody was reading is gone for good.
const cdpDetail = `Chrome DevTools Protocol: the browser's own JSON-RPC over a socket.

  open <session> cdp <url>          ws://host:port/devtools/browser/<id>
                                    — or the http:// endpoint, whose /json/version
                                    is read for the socket (a bare host:port works)
  ls <session> cdp                  the connections, with their addresses
  ls <session> cdp:1                the tabs in one connection
  open <session> cdp:1 --target <id>   attach one tab → <session> cdp:1@<targetId>
  call <session> cdp:1@<id> '<frame>'  a page-level method; the sessionId is filled in
  call <session> cdp:1 '<frame>'       a browser-level method (Target.*, Browser.*)
  get <session> cdp:1               the connection: endpoint, cursor, attached tabs
  get <session> cdp:1@<id>          one tab: target, url, enabled domains
  get <session> cdp:1 protocol [Dom]   the browser's OWN command list, live
  read <session> cdp:1 --since N    frames after N (does not consume them)
  close <session> cdp:1@<id>        detach that tab; the tab stays open
  close <session> cdp:1             close that connection

TWO NUMBERS, TWO DIFFERENT THINGS.

  cdp:1   WHICH CONNECTION. One per browser you opened, numbered in the order they
          were opened, never reused. Two browsers on one machine are cdp:1 and
          cdp:2 — that is the whole reason the number exists.
  @<id>   WHICH TAB inside it. That is the browser's own targetId, the one
          Target.getTargets prints, so it can be pasted straight back into a frame.
          Pages are NOT numbered here: the browser already names them, and a second
          numbering scheme would only give you two names for one thing.

One connection manages the whole browser instance, so you do not need one per tab.
With exactly one connection open, the number can be left out (internal cdp);
with two, an unqualified address is refused rather than guessed — driving the wrong
browser while the reply says it worked is the failure this avoids.

NO HANDSHAKE. DevTools is not MCP and answers initialize with "method not found";
this door never sends one. There is also no session header: CDP's sessionId goes
INSIDE the frame, and a frame without one is addressed to the browser process.

START A BROWSER, then open it:

  open internal --argv chrome --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 about:blank
  open internal cdp http://127.0.0.1:9222

--remote-debugging-address=127.0.0.1 KEEPS THE PORT OFF EVERY OTHER INTERFACE.
Without it Chrome listens on 0.0.0.0 and the operating system asks the human to
approve a firewall rule — for a throwaway debug port. The address is 127.0.0.1
because the port is only ever reached through the session's loopback anyway: a
remote browser is reached by tunnelling through the SSH link, not by exposing the
port.

--remote-debugging-port=0 writes the chosen port to DevToolsActivePort in the
profile directory; read it with file and open that. Port 9222 is fine when it is
free, which is the usual case for a throwaway probe.

A TAB IS A SESSION. The browser itself has three useful methods —
Target.getTargets, Target.createTarget, Target.attachToTarget — and everything else
(Page.*, Runtime.*, DOM.*) needs the sessionId that attach returns. Attaching is a
step in the tree, like adding a channel to a session:

  ls internal cdp:1
  open internal cdp:1 --target 9412C84B213728693D61314D75BDEC45
      -> Handle: internal cdp:1@9412C84B213728693D61314D75BDEC45

EVERY LATER FRAME GOES THROUGH THE TAB ADDRESS, which fills in the sessionId:

  call internal cdp:1@9412C84B… '{"method":"Page.enable"}'
  call internal cdp:1@9412C84B… '{"method":"Page.navigate","params":{"url":"https://…"}}'

Writing that sessionId by hand works too — a frame that already has one is left
alone — but it is the one mistake that is SILENT: a page-level method sent to the
browser process is answered with "wasn't found", and a Page.enable sent before the
attach succeeds against the browser and delivers nothing, forever.

ONE-SHOT. A single command that does not deserve an attach step can name the target
without attaching first: call internal cdp@<id> '{"method":"Page.reload"}'. The
session is created, used, and detached; nothing is left behind, and nothing that was
already attached is touched.

flatten:true is what makes the sessionId live in the frame; without it CDP routes by
a separate id on the socket and every frame here would have to be re-wrapped. The
door sets it for you.

WAITING FOR A PAGE IS A QUESTION ABOUT THE PAST, which is why the frames are kept.
Page.loadEventFired fires AFTER Page.navigate has already answered, and CDP never
replays an event — so a reader that was not looking at that moment needs a window to
look back into:

  get internal cdp:1               -> Latest: 42   (record it BEFORE the action)
  call internal cdp:1@<id> '{"method":"Page.navigate",…}'
  read internal cdp:1 --since 42   -> 43 event Page.frameStartedNavigating …
                                      44 event Page.loadEventFired …

read <session> cdp:1 --since N does NOT consume: asking again from the same N gives
the same frames, so a caller resumes from the last number it read rather than
guessing. --grep filters before the window. A plain read is the other thing — the
next frame off the queue, once — and it is what send pairs with.

One connection carries every attached tab's frames interleaved, each tagged with its
sessionId. A tab address narrows the read to that tab; the bare connection shows the
browser-level frames too.

Enable the domain first, or the event never fires at all:
Page.setLifecycleEventsEnabled (or just Page.enable) before navigating.

A BIG SCRIPT NEVER TRAVELS THROUGH THE TOOL. The frame is the command line, so a
200 KB bundle would have to be emitted by the caller — the one thing that does not
scale. Let the browser fetch it: serve the file from the session's own network, then
append a script element pointing at the URL.

  open internal --argv python3 -m http.server 8123 --directory /srv/assets
  call internal cdp:1@<id> '{"method":"Runtime.evaluate","params":{"expression":
     "var s=document.createElement('script');s.src='http://127.0.0.1:8123/app.js';document.head.appendChild(s)"}}'

Only the URL crosses the tool. The server is an ordinary pipe channel, and 127.0.0.1
is the browser's own loopback — the file never has to leave the machine it is on.
Raw CDP has no addScriptTag (that is Puppeteer's helper), which is why the frame
builds the element itself.

The same shape injects before load, with the source inline instead of a URL:
Page.addScriptToEvaluateOnNewDocument. That one does put the source in the frame, so
it is for small snippets.

A BIG ANSWER IS A RECEIPT, NOT A WALL OF TEXT. Anything over 4 KB does not come back
inline — the reply names the frame and shows its first bytes, because a JSON document
cut in the middle is too big to have been worth the tokens and too broken to use. The
frame is in the log either way, so page it:

  call internal cdp:1@<id> '{"method":"Runtime.evaluate","params":{"expression":"document.documentElement.outerHTML"}}'
      -> Bytes: 46882  Frame: 57
         frame 57 is 45.8 KB, too big to inline. … first 511 bytes: …

  read internal cdp:1 --at 57 --offset 0      the first 4 KB
  read internal cdp:1 --at 57 --offset 4096   the next 4 KB
  read internal cdp:1 --at 57 --offset 4096 --bytes 512    a smaller page

--at pages INSIDE one frame; --since pages forward in TIME. Offsets are bytes of the
frame as the log holds it (the reply reports Bytes for that size, Decoded-Bytes for
the same frame as text, then Offset and Next-Offset), so continuing is copyable. A
slice never ends in the middle of a character or of a \uXXXX escape: the boundary
backs off, which is why a page can be a byte or two short of what you asked for.

A SCREENSHOT DOES NOT COME BACK AS TEXT. Page.captureScreenshot answers with
base64, and a 150 KB blob in a reply is neither readable nor cheap. Until the pipe
lands, take the frame from the log and hand it to a file yourself:

  call internal cdp:1@<id> '{"method":"Page.captureScreenshot","params":{"format":"png"}}'
      -> Bytes: 148231  Frame: 91
  read internal cdp:1 --at 91 --offset 0 --bytes 4096     (page it out yourself)`
