package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it TERSE: many clients re-send it every
// model turn, so each rule is paid for repeatedly.
//
// WHAT BELONGS HERE AND WHAT DOES NOT. This file answers one question: which facts
// must already be in context BEFORE the model's first call, because the model cannot
// discover them by acting? Only four qualify — the tool is the whole surface, a
// handle cannot be guessed, a human boundary is destructive to cross, and a held
// reply must not be retried. Everything else is discoverable exactly when it is
// needed: the command space by `help`, a directive's traps by `help <directive>`,
// the window parameters by the tool schema, and the reply format by the reply
// itself. Restating those here would be a second description to keep in sync, and
// (worse) a fixed per-turn cost for facts most sessions never use.
//
// The tests in docs_test.go and token_budget_test.go hold this line in both
// directions: the hard boundaries must stay, and the directive table must not
// migrate back into the text.

const mcpServerInstructions = `termcp agent rules:

0) One tool: term(cmd), a terminal line: <directive> <handle> <payload>. No cmd = bearings (where am I, what runs, what is installed); help lists every directive, help <directive> teaches one. Guides: api.md.
1) Handles are <session>:N (a channel in a connection) — never invent one, copy the first column of ps. termcp:// locators work wherever a handle does.
2) Human in the loop (HARD BOUNDARY): notify FIRST (--level warn|error --sticky --on <session>) before any password/sudo/passphrase/MFA, confirmation, approval, choice or interactive input, and say it in your reply. Never guess, paste, echo or store secrets; never bypass sudo/auth/approval (sudo -n, NOPASSWD/setuid/sudoers, reword, encode, wrap, retry loops). Human at a prompt: notify, then watch --event output on that handle — waiting is not a stall, but polling is.
3) review_pending=true: held, nothing ran, no id to poll — do NOT resubmit, reword or retry it. notify (rule 2), then wait: staged text queues when its ending key arrives, and a later read shows whether it ran.
4) Put newlines/binaries in the vars argument and reference $NAME in cmd; raw control bytes need allow_escapes:true. Other keys use JSON \u001b escapes in type.`
