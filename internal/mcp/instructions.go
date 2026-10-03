package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it terse because clients may include it
// in every model turn. It is a fixed rule set and never carries this instance's
// address: `instructions` is optional in MCP and often discarded, so the address
// rides on the notify_user tool description instead (see decorateTools).
const mcpServerInstructions = `termcp agent rules:

0) Tools: session_*/shell_* are standalone; forward/message/ssh_config and file_perm/file_link/file_fs take an "action" parameter (enum in each schema).
1) IDs: session_id = connection container (forwards, files, terminate, shell_open); shell_id = terminal channel (input/key/output/resize/close, readers). Never invent them; take from session_start / shell_open / list tools. termcp:// locators from the user ("termcp://mac" entry, "termcp://#<sid>" session, "termcp://#<sid>:N" shell) are accepted in place of ssh_config names, session_id, shell_id — use verbatim, no lookups.
2) Mode: interactive shell (omit command/args, DEFAULT) for multi-step/stateful work; drive via shell_input + shell_key(enter) + shell_output(timeout≤3) loops. shell_output returns ONLY new bytes: empty ≠ done, keep polling. Dedicated command (command/args set) ONLY for REPL/TUI, daemons, or one atomic script. Never split sequential steps into session_start(bash -c) calls (loses cwd/env, wastes handshakes).
3) After discovery, act with concrete calls, not prose. Verify success via output or an explicit success field.
4) Lifecycle: session_terminate closes a session (shells + SSH + forwards) but keeps it in the registry as exited/DEAD, output still readable via shell_output; session_delete erases it for good. force=true = immediate kill. shell_close closes one channel only.
5) Human input: before any password/sudo/passphrase/MFA, confirmation, approval, choice, or interactive input, call notify_user with warn/error + duration_seconds=0 + session_id, then stop and ask the human in the termcp Web UI. Never guess, paste, or echo secrets.
6) Other keys use JSON \u001b escapes in shell_input. Repeating traceback → session_terminate, retry with PYTHON_BASIC_REPL=1. Silent hang → session_info.
7) forward(action=local/remote/dynamic) = ssh -L/-R/-D, all take session_id. ssh_config(action=list) only returns names; never expose credentials.
8) shell_notify(action=register, shell_id, channel="resource"|"sampling", event="output"|"exit"|"silence") = async wake-up (no payload); poll shell_output when woken.
9) notify_user(message, level, session_id?) reaches the human's Web UI (not the Agent): call it before any ask and on long-task end/failure; use warn/error + duration_seconds=0 for blockers. shell_notify wakes the Agent.`
