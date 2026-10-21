// The jrpc/directive detail pages.
//
// They are separate from mcpDetail on purpose, and they are SHORT. mcpDetail is
// long because the MCP handshake, the two handle kinds and the frame quoting are
// each a trap; `jrpc` has none of those — it moves frames and says so — and a
// caller that reads a page three times the size of the contract learns nothing
// extra. cdpDetail is long for a different reason: the traps there are the
// browser's (which URL to open, how a tab is addressed), not the protocol's.
package directive

// jrpcDetail is the page for the door that adds nothing.
const jrpcDetail = `A raw JSON-RPC frame channel. Nothing here interprets a method: what you send is
what the peer receives, and what it answers is what you get back. That is the
whole contract, and it is why any peer that speaks the envelope works — an LSP
server, a private RPC, a device.

  jrpc open <url>            ws:// wss:// http:// https:// — scheme picks transport
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
// anything, and that a tab is addressed by a sessionId which is set AFTER the
// page tools stop working — so the enable calls must follow the attach, not
// precede it.
const cdpDetail = `Chrome DevTools Protocol: the browser's own JSON-RPC over a socket.

  cdp open <url>          ws://host:port/devtools/browser/<id>
                          — or the http:// endpoint, whose /json/version is read
                          for the socket (a bare host:port works, no path needed)
  cdp call <handle> '{"method":"Target.getTargets"}'
  cdp send <handle> '<frame>'      no answer expected
  cdp recv <handle>                a pushed event
  cdp close <handle>

NO HANDSHAKE. DevTools is not MCP and answers initialize with "method not found";
this door never sends one. There is also no session header: CDP's sessionId goes
INSIDE the frame, and a frame without one is addressed to the browser process.

START A BROWSER, then open it:

  pipe internal --argv chrome --headless=new --remote-debugging-port=9222 about:blank
  cdp open http://127.0.0.1:9222

--remote-debugging-port=0 writes the chosen port to DevToolsActivePort in the
profile directory; read it with file and open that. Port 9222 is fine when it is
free, which is the usual case for a throwaway probe.

A PAGE IS A SESSION. The browser itself has three useful methods —
Target.getTargets, Target.createTarget, Target.attachToTarget — and everything
else (Page.*, Runtime.*, DOM.*) needs the sessionId that attach returns:

  cdp call $h '{"method":"Target.createTarget","params":{"url":"about:blank"}}'
  cdp call $h '{"method":"Target.attachToTarget","params":{"targetId":"<id>","flatten":true}}'
      -> {"result":{"sessionId":"<sid>"}}

EVERY LATER FRAME CARRIES IT:

  cdp call $h '{"sessionId":"<sid>","method":"Page.enable"}'
  cdp call $h '{"sessionId":"<sid>","method":"Page.navigate","params":{"url":"https://…"}}'

Attach BEFORE enabling: a sessionId is minted by attachToTarget, so the
Page.enable that must precede any Page event cannot be sent until then. Getting
this order wrong is silent — the calls succeed against the browser and no page
event ever arrives.

flatten:true is what makes the sessionId live in the frame; without it CDP routes
by a separate id on the socket and every frame here would have to be re-wrapped.

WAITING FOR A PAGE is the pattern events exist for, and recv is the wait.

  cdp call $h '{"sessionId":"<sid>","method":"Page.navigate","params":{"url":"…"}}'
  cdp recv $h --timeout…   (the window is the call's timeout parameter)

recv BLOCKS for up to the call's timeout and returns the first frame that carries
no id — a pushed event — or 204 when the window expires with nothing. It is not a
peek at what some earlier call happened to catch: Page.loadEventFired fires after
navigate has already answered, and a recv issued afterwards reads the socket and
waits for it. Give it room; the default two seconds is short for a real page.

Enable the domain first, or the event never fires at all:
Page.setLifecycleEventsEnabled (or just Page.enable) before navigating.`
