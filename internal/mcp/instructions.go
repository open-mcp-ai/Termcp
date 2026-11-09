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

TERM/1 replies: <code>, state headers (Waited, Content-Range, Still-Running,
Error-Code), body.

cmd = the DSL; sh = a shell command. cmd is parsed as DSL, so shell text enters only
as a VALUE ($1, $NAME): /tmp/$1 would be read by two grammars. && || ; & compose cmd.

Tree: host -> session -> channel/file/forward/peer. 'connect <host>' opens one;
'run/read/write use:<host>' target the pool directly; 'help [verb]' the usage.

MODE: pipe (--argv) = no echo, observable exit — scripts, batch; pty = TUI, prompts,
no exit signal, so its reply is a sample.

CAPABILITY — start at 'ls ext': installed MCP servers and skill trees,
with their operating notes. Search here first for a tool or a skill. 'ls': session
state (learn_mode:true adds usage). MCP peer: 'get <session> mcp tools', then
'call <peer> <tool> {json}'. CDP controls a real browser: 'open <session> cdp <url>',
then 'call <peer> {method}' — navigate, click, screenshot, eval. Repeated job?
'create ext', 'create alias <name> <cmd>' or 'open <session> mcp <url>'.

A HUMAN IS AN APPROVAL CHANNEL
  notify reaches open Web UI pages and delivers to a person; an answer arrives only as
  their reply or as channel bytes — delivery is not acknowledgement.
  password, sudo, MFA, confirmation -> 'notify', then wait for that answer;
  polling asks a page, not a person.
  Review mode: review_pending=true (approval_id) -> HELD for a human decision;
  resending queues a second, not a release.
  cmd/args -> server log verbatim; secrets -> names only; shell echoes what it runs.
  Long, awkward, missing a capability, or maybe wrong way?
  say what you are trying to do and ask while a human can answer.`
