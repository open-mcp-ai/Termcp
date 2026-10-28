# Access policy: tokens, gates, and reviewed intents

How Termcp decides what a caller may do, and how a human or another agent can
decide it instead. This is the design for the permission layer; it builds on the
capability table that already exists (`internal/capability`) rather than
replacing it.

**Summary.** There is no "human" or "AI" in this model — only a token and the
permission template that token's *signer* is bound to. Every operation passes a
chain of gates, each answering allow, deny, or review. A reviewed operation is
frozen into an intent closure — arguments already expanded, target already
resolved — and executed by the daemon only if a reviewer says yes, so nothing the
requester supplies can change between the decision and the act.

---

## 1. Actors and tokens

`admin` and `agent` are not kinds of principal. They are templates, and a
template is chosen by *which key signed the token* — never by a claim the signer
wrote.

| Signer key | Template | Who realistically holds it |
| --- | --- | --- |
| `root.key` | built-in MCP template | any process running as the daemon's user |
| `human.key` | human template; scopes come from the token record | whoever knows the passphrase |
| (none) | rejected | — |

### 1.1 The rule that makes self-signing safe

**Scope is derived from the signer, not read from the token.**

A caller that reads `root.key` can mint a token claiming `scopes: ["*"]`; the
daemon ignores the field entirely and answers with the built-in MCP template.
This is the whole reason local self-signing is acceptable: reading the root key
is something only the daemon's own OS user can do, and that user already has a
shell on this host, so the token grants nothing they did not already have.

The invariant that keeps it true:

> Human authority is never reachable with `root.key` alone.

`human.key` is a second key, sealed by the passphrase (Argon2id → AES-GCM). The
daemon stores a verifier and the two public keys; it holds no plaintext secret
for the human side.

**If both templates were signed by one key, the hole returns**: an agent with
`file.read` would read the key and mint a human token. The separation is the
design, not a detail.

### 1.2 Claims are clamped, not trusted

Even inside the MCP template, the daemon does not believe the client's claims:

| Claim | Treatment |
| --- | --- |
| `scopes` | ignored; replaced with the template's set |
| `sub` | recorded as *unverified* and prefixed `local:`, so audit cannot be spoofed into looking like a named agent |
| `exp` | rejected if further out than `now + max_local_ttl` (default 1h) |
| `aud` / `iss` | pinned |
| `alg` | pinned to the key's algorithm; `none` and cross-family confusion rejected |
| `jti` | recorded; a repeat is refused (replay) |

### 1.3 Key layout

```
<data-dir>/access/
  root.key         Ed25519 private, 0600. Local callers sign with it.
  root.key.meta    kid, algo, created_at
  human.key        Ed25519 private, sealed by the passphrase
  passwd           Argon2id(passphrase) verifier
  tokens.json      issued-token metadata: kid, jti, sub, scopes, exp, revoked
```

Resolution order for the key file: `--root-key-file` → `$TERMCP_ROOT_KEY_FILE`
→ the default above. **Only a path is accepted**; an inline key requires an
explicitly different name (`TERMCP_ROOT_KEY_INLINE`) and logs a warning at
startup, because an inline secret leaks into `/proc/1/environ`, crash dumps and
`docker inspect`.

In a container the secret is mounted read-only:

```yaml
environment:
  TERMCP_ROOT_KEY_FILE: /run/secrets/termcp-root.key
secrets:
  - termcp-root-key
secrets:
  termcp-root-key:
    file: ./secrets/root.key
```

Read-only matters for a specific reason: a writable key file can be **replaced**
with one the attacker holds, so the next daemon start trusts every token that
attacker signs.

### 1.4 Rotation is the only revocation for a leaked key

A copied `root.key` stays valid forever. There is no way to distinguish a token
it signed from a legitimate one, so the response to a suspected leak is
`termcp token rotate-root`: new key material, new `kid`, old `kid` marked
revoked in `tokens.json`. Every token carries `kid`, so the refusal is exact.

Because key reads cannot be logged, **token use is logged instead**: `kid`,
`jti`, `sub`, first capability. A token minted from a stolen key shows up there,
which is the only place it can.

### 1.5 First run and the claim window

With no `passwd` on disk the instance is uninitialized, and:

> An uninitialized daemon **refuses to bind a non-loopback address.**

The window between start and the first login is a takeover window — whoever
arrives first sets the passphrase. Binding to loopback is not enough as a rule,
because `--host 0.0.0.0` can be passed before anyone logs in; refusing the bind
is a rule that cannot be misconfigured.

---

## 2. Scope grammar and preset templates

A template is a named, versioned bundle of scope strings.

```
dsl:<verb>[:<kind>]           dsl:run, dsl:open:ext, dsl:write:file
api:<area>:<action>           api:read, api:session:delete
ext:<name>[:<tool-glob>]      ext:winbox, ext:adbox:screen_*
review:<right>                review:inspect, review:decide, review:escalate
```

Globs are allowed but discouraged: a template containing `*` cannot be answered
statically, and "what can this token do" is the question a reviewer asks.

Presets:

| Preset | Contents |
| --- | --- |
| `mcp` (built-in) | everything the capability table registers for the `term` entrance and the peer/extension surface — today's behaviour |
| `mcp:ops` | `dsl:ls`, `dsl:open:ext`, `dsl:call`, `dsl:read` (peer only), `ext:<whitelist>`; **no** `dsl:run`, `dsl:input`, `dsl:write`, `dsl:rm`, `dsl:forward` |
| `human` | the Web UI's own routes and the full DSL |
| `reviewer:smart` | `review:inspect`, `review:decide`; escalation needs `review:escalate` |
| `readonly` | `api:read`, `dsl:ls`, `dsl:get`, `dsl:read` |

**A newly added capability belongs to no template by construction.** Templates
are explicit lists; the zero answer is "not granted". This is the same
closed-by-default rule the capability table already follows.

---

## 3. Gates

A gate is one function answering, for a `(subject, capability, target, args)`
tuple:

```go
type Decision int
const (
    Allow Decision = iota
    Deny
    Review
)

type Gate interface {
    Decide(ctx context.Context, s Subject, a Action) (Decision, Reason)
}
```

### 3.1 Where gates attach

| Point | Sees | Can prevent |
| --- | --- | --- |
| HTTP entrance | route, method, claims | yes |
| DSL entrance | verb, target, kind | yes |
| DSL sub-command | sub-action, flags | yes |
| Capability layer | `capability.ID` | yes |
| Extension tool call | extension name, tool name, arguments | yes |
| **DSL exit** | the produced envelope | **no** |

The last row is a limit, not a feature: by the time an operation has produced a
reply its side effects are committed. An exit gate is an **observer** — it can
audit, annotate, or trigger a follow-up, and must never be described as
prevention.

### 3.2 Precedence

The chain is evaluated with three rules, and the order matters because this is
where permission systems usually go wrong:

1. **Deny always wins.**
2. **Review beats allow.**
3. **No gate and no default ⇒ deny.** An evaluation that errors, times out, or
   finds an unregistered capability is a refusal, not a pass.

**Gates narrow, never widen.** The token's template is the ceiling; a gate can
remove from it and cannot add. Otherwise a misconfigured gate becomes an
escalation.

### 3.3 The entrance does not decide authority

`POST /api/term` is reachable by both a browser and an agent, so the route cannot
be what determines the permission set. The subject's template decides; the route
only appears in the audit record. Making the route authoritative is the classic
confused-deputy mistake.

---

## 4. Intent closures

A reviewed operation is not stored as text to be re-parsed later. It is frozen:

```go
type IntentClosure struct {
    ID        string        // rev_7f39a01b
    CreatedAt time.Time
    ExpiresAt time.Time     // expiry is a destruction, not a grace period
    Caller    SubjectSnapshot // token id, kid, sub, template — NOT the reviewer's authority
    Capability string
    Target    TargetRef      // bound to the resolved connection, not a name that can be rebound
    Args      map[string]any // every $N and $VAR already expanded to literals
    WorkDir   string         // absolute, captured at freeze time
    Digest    string         // hash over everything above
    Execute   func(context.Context) (envelope.Slot, error)
}
```

Four rules:

1. **Freeze after expansion.** Reviewing a command that still contains `$1` is
   reviewing a hole: the approved text and the executed text would differ.
2. **Bind the target.** The closure holds the resolved session/connection, not a
   hostname that could resolve elsewhere five minutes later.
3. **Execute with the caller's authority**, never the reviewer's. A reviewer
   approving a `root` operation must not lend their own privilege to it.
4. **No replay.** The closure is removed on execution, timeout, or rejection;
   the `Digest` is recorded so a replay attempt is detectable rather than
   silently executed twice.

Anything that truncates the payload for display must fail closed: a reviewer who
cannot see the entire frozen argument set is not reviewing it.

---

## 5. Review chain

Reviewers are subjects with `review:decide`, and nothing about the mechanism
cares whether the subject is a person's browser session or an MCP extension.

### 5.1 Disjoint identity is required

**A reviewer must not be reachable by the requester.** If the agent can drive the
extension that reviews its own requests, the gate is decoration. Concretely: the
reviewer's token is not in the requester's template, and a reviewer extension is
never one the requester may `call`.

### 5.2 An agent reviewer is a second opinion, not a guarantee

An intelligent reviewer is a different decision procedure — worth having, and
worth stating honestly:

- It can approve the ordinary case, so a human is not paging through routine work.
- It is **not** a proof of safety: it is one more model reading the same text.
- It can escalate: `review:escalate` moves the closure to a higher reviewer with
  its analysis attached, which is where a human first sees the request.

Through MCP `sampling/createMessage` a reviewer extension can borrow the host
client's model instead of carrying its own API credentials.

### 5.3 The chain terminates

`escalate` needs a depth budget (default 2) and the top must be declared
terminal. Without both, two reviewers can bounce a request between themselves
indefinitely, or a reviewer can escalate to itself.

### 5.4 Unavailability fails closed

A reviewer that is offline, whose token expired mid-review, or that never
answers means **no execution**. The refusal names which of those it was —
"the reviewer is unreachable" and "a reviewer denied this" send an agent to two
different places.

---

## 6. Standing grants

Per-request approval does not survive contact with operations: nobody clicks
approve two hundred times a day, and an operator who does starts clicking
without reading.

A standing grant is:

```
capability:  ext:winbox:service_control
pattern:     services matching "nginx|redis"
ttl:         30m
reviewer:    human:alice
revocable:   yes, immediately; revocation invalidates in-flight approvals
```

Time-boxed, pattern-scoped, attributed to the reviewer, and revocable. The audit
record shows which requests ran under a grant rather than an individual decision.

---

## 7. Fail-closed checklist

Each of these is a bug that silently removes the protection above it:

- Unknown capability ⇒ deny.
- Gate error or timeout ⇒ deny.
- Reviewer unavailable or expired ⇒ deny.
- Truncated payload ⇒ deny (the reviewer cannot see it).
- Missing or unparsable token ⇒ deny.
- `alg: none`, unknown `kid`, revoked `jti` ⇒ deny.
- Uninitialized instance with a non-loopback bind ⇒ refuse to start.

And one that is easy to miss:

> **The subject must not be able to read the signing keys.** `file.*` on
> `<data-dir>/access/` is denied by default, and the directory is outside every
> extension path whitelist. If an agent can read `root.key` it signs itself a
> token — capped at the MCP template by §1.1, but that is still the whole reason
> the two-key split exists.

---

## 8. Implementation map

| Concern | Where |
| --- | --- |
| token parse, claims, kid→template | `internal/auth` (extend `Verifier`) |
| scope grammar, templates | new `internal/policy` |
| capability → scope mapping | `internal/capability` (add the missing `dsl`/`review` rows; today `call`/`read` on a peer has no row at all) |
| gate chain | new `internal/gate`, called from `runDirective` and `checkAdmission` |
| closures | new `internal/intent`, sibling of `internal/task` |
| review queue | `internal/approval` (exists: per-session, single decision, fails closed) |
| Web UI token issuance | `internal/webui` |
| Web UI review inbox | `internal/webui` |
| CLI | `termcp token …` (create/list/revoke/rotate-root) |

### 8.1 Two gaps in the current table

- `call <peer>` and `read <peer>` have **no capability id**: only `mcp.open`,
  `jrpc.open` and `cdp.open` exist. A policy that allows "use extensions" has
  nothing to name for "use them", so `peer.call` / `peer.read` must be added.
- `refuseAction` fires only for some verbs, and needs a resolved session. An
  instance-level policy must be consulted where every statement passes —
  `runDirective` — not only where a session happens to be named.

---

## 9. Open questions

1. Does `mcp:ops` need `dsl:close` (close an extension channel) or is leaking
   idle channels acceptable?
2. Standing grants: is the pattern evaluated against the frozen arguments (safe,
   static) or against live state (useful, and a TOCTOU of its own)?
3. Should `review:decide` be splittable — approve-only vs deny-only — so an
   aggressive reviewer can stop things without being able to start them?
4. Audit log signing: are decisions recorded with a per-entry signature, or is
   the log trusted because it is append-only on the host?
