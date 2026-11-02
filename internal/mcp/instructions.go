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

cmd = the DSL; sh = a shell command. sh enters cmd only as a VALUE ($1, $2, $NAME),
never as syntax (/tmp/$1 refused). cmd composes with && || ; &.

Tree: host -> session -> channel/file/forward/peer. connect <host> opens one;
run/read/write use:<host> … borrow the pool and leave no session. ls lists
addresses; copy, don't invent. help [verb] has usage.

MODE: pipe (--argv) = no echo, observable exit — use it for scripts and batch
work; pty = TUI and prompts, no exit signal, so its reply is only a sample.

CAPABILITY — inspect first. ls ext lists resident extensions and their notes;
ls lists state (learn_mode:true adds usage). MCP: get <session> mcp tools, then
call <peer> <tool> {json}. CDP: call <peer> {method}. Repeated job? create ext or
open <session> mcp <url>.

A HUMAN IS AN APPROVAL CHANNEL
  notify FIRST before any password/sudo/passphrase/MFA, confirmation, approval, choice
or input. Review mode exists so a human sees each capability being granted; guessed,
echoed or stored secrets — or a bypassed gate (sudo -n, NOPASSWD, setuid) — hide it.
Keep secrets out of cmd/args; remote echo enters the transcript.
At a prompt: notify, then WAIT; read slow tasks in pieces, not by polling.
A long, awkward call or a capability that should exist but does not?
say what you are trying to do and ask while a human can answer.

REVIEW — review_pending=true means HELD: nothing ran. The reply's approval_id is
for pairing it with the decision later; it is not authority. Do NOT resubmit or
retry, and do not poll; notify, then wait.
`
