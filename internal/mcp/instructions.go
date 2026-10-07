package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it terse because clients may include it
// in every model turn. It is a fixed rule set and never carries this instance's
// address: `instructions` is optional in MCP and often discarded, so the address
// rides on the notify_user tool description instead (see decorateTools).
//
// Rule 6 is the one rule about review mode that belongs here: the reply it
// describes (review_pending) answers a call the model just made, so the rule has
// to be in context before that call. It names rule 5 instead of repeating its
// notify_user parameters. The tool RESULT carries the operational half at the
// moment it matters (see reviewPendingReply); that repetition is deliberate,
// because a model that meets the reply mid-session reads the reply, not a rule
// from the start of the session.
//
// Rule 9 keeps only what is unique to it: old rule 9 restated rule 5's notify_user
// rule ("call it before any ask", blockers as warn/error + duration_seconds=0), so
// that half was dropped rather than said twice.
const mcpServerInstructions = `termcp agent rules:

0) Tools: session_*/shell_* standalone; forward/message/ssh_config and file_perm/file_link/file_fs take an "action" (enum per schema).
1) IDs: session_id = connection (forwards, files, terminate, shell_open); shell_id = channel (input/key/output/resize/close, readers). Never invent them; take from session_start/shell_open/list. User termcp:// locators replace the argument they name ("termcp://<entry>"=ssh_config, "termcp://#<sid>"=session_id, "termcp://#<sid>:N"=shell_id): verbatim, no lookups.
2) Mode: interactive shell (omit command/args, DEFAULT) for multi-step work; drive via shell_input + shell_key(enter) + shell_output(timeout<=3). shell_output returns ONLY new bytes: empty != done, keep polling. command/args ONLY for REPL/TUI, daemons or one atomic script; never split steps into session_start(bash -c) calls (loses cwd/env).
3) After discovery, act with concrete calls, not prose. Verify success via output or an explicit success field.
4) Lifecycle: session_terminate closes shells + SSH + forwards, entry stays as exited/DEAD, output readable via shell_output; session_delete erases it. force=true = immediate kill; shell_close closes one channel only.
5) Human in the loop (HARD BOUNDARY): notify_user FIRST (warn|error, duration_seconds=0, session_id) before any password/sudo/passphrase/MFA, confirmation, approval, choice or interactive input, and say it in your reply. Never guess, paste, echo or store secrets; never bypass sudo/auth/approval (sudo -n, NOPASSWD/setuid/sudoers, reword, encode, wrap, retry loops).
6) review_pending=true: held, nothing ran, no id to poll - do NOT resubmit, reword or retry it. notify_user (rule 5), then WAIT (not a stall): staged text queues when its ending shell_key arrives, and shell_output later shows whether it ran.
7) Other keys use JSON \u001b escapes in shell_input. Repeating traceback: session_terminate, retry with PYTHON_BASIC_REPL=1. Silent hang: session_info.
8) forward(action=local/remote/dynamic) = ssh -L/-R/-D, all take session_id. ssh_config(action=list) only returns names; never expose credentials.
9) shell_notify(action=register, shell_id, channel="resource"|"sampling", event="output"|"exit"|"silence") = async wake-up (no payload); poll shell_output when woken. shell_notify wakes the Agent, notify_user the human. Human at a prompt: notify_user, then register event=output.`
