package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it TERSE: many clients re-send it every
// model turn, so each rule is paid for repeatedly.
//
// IT DESCRIBES THE WORLD, AND THE WORLD IS SMALL ENOUGH TO DESCRIBE.
//
// Five drafts led here. The first was four numbered rules, three of them "never do X":
// a reader finished knowing every way to fail and nothing about what it held. The
// second described the terrain in prose — correct but wordy. The third was the full
// directive table, which is right in an on-demand guide and wrong here. The fourth
// carried a HARD BOUNDARY section that listed six forbidden tricks (sudo -n, NOPASSWD,
// setuid …) and no reason: a reader learned what not to type without learning what the
// boundary protects or what to do instead.
//
// This one is ordered the way a caller needs it, and every refusal it prints carries
// its reason: what the thing is, what a call carries, where capability lives (including
// where to ADD it), and what a human is for. The writing rules behind that shape are in
// docs/agents/hints.md.
//
// SYNTAX LIVES IN `help`, NOT HERE. A directive's usage line, its flags and its traps
// are one call away and change with the implementation; restating them here would cost
// every turn and drift. What stays is what a model must know BEFORE it can decide to
// make that call.
//
// The tests in docs_test.go and token_budget_test.go hold this line in both
// directions: the human boundary and the review rule must stay, and the directive table
// must not migrate back into the text.

const mcpServerInstructions = `termcp — a shell-like DSL over SSH; local ("internal") = remote.

Replies are HTTP-like: TERM/1 <code>, then state headers (Waited, Content-Range,
Still-Running, Error-Code) and a body.

cmd = the DSL; sh = a shell command. A shell line reaches cmd only as a referenced
VALUE ($1, $NAME), because cmd is parsed as DSL: /tmp/$1 would be read by two
grammars at once. cmd composes with && || ; &.

Tree: host -> session -> channel/file/forward/peer. connect <host> opens one;
run/read/write use:<host> … borrow the pool, leave no session. ls prints the
addresses that exist; help [verb] the usage.

MODE: pipe (--argv) = no echo, an observable exit — scripts, batch work; a pty =
TUI and prompts, no exit signal, so its reply is only a sample.

CAPABILITY — inspect first. ls ext lists resident extensions and their notes;
ls lists state (learn_mode:true adds usage). MCP: get <session> mcp tools, then
call <peer> <tool> {json}; CDP: call <peer> {method}. Repeated job? create ext or
open <session> mcp <url>.

A HUMAN IS AN APPROVAL CHANNEL
  termcp cannot ask a person: notify reaches open Web UI pages, and delivery is not
 acknowledgement — an answer comes back only as the human's reply or as channel
 bytes. A password, sudo, MFA or confirmation therefore goes out through notify and
 the wait is for that answer; polling asks a page, not a person. Review mode lets a
 human see each capability granted: review_pending=true means HELD, nothing ran;
 approval_id pairs the request with the decision, and resending queues a second
 rather than releasing the first. cmd/args reach the server log verbatim, secrets
 logs names — and the remote shell echoes what it runs anyway.
 Long, awkward, or missing a capability?
 say what you are trying to do and ask while a human can answer.
`
