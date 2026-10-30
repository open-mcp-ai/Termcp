package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func (s *Server) handleCreateSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	host := strings.TrimSpace(getString(args, "host", ""))
	user := strings.TrimSpace(getString(args, "user", ""))
	password := strings.TrimSpace(getString(args, "password", ""))
	privateKey := strings.TrimSpace(getString(args, "private_key", ""))
	keyPassphrase := strings.TrimSpace(getString(args, "key_passphrase", ""))
	port := int(getFloat64(args, "port", 22))
	trustUnknown := getBool(args, "trust_unknown_host", false)
	knownHosts := strings.TrimSpace(getString(args, "known_hosts", ""))
	dialTimeout := int(getFloat64(args, "dial_timeout_seconds", 30))
	proxy := strings.TrimSpace(getString(args, "proxy", ""))
	description := strings.TrimSpace(getString(args, "description", ""))
	defaultShell := strings.TrimSpace(getString(args, "default_shell", ""))
	defaultMode := strings.TrimSpace(getString(args, "default_mode", ""))
	// On create an absent field is simply "off". The presence check that matters
	// is on edit, where it distinguishes "turn it off" from "do not touch it".
	defaultApproval := getBool(args, "default_approval", false)

	entry := &sshconfig.Entry{
		Kind:            sshconfig.KindRemote,
		Description:     description,
		DefaultShell:    defaultShell,
		DefaultMode:     defaultMode,
		DefaultApproval: defaultApproval,
		DialSpec: sshconfig.DialSpec{
			Host:               host,
			Port:               port,
			User:               user,
			Password:           password,
			PrivateKey:         privateKey,
			KeyPassphrase:      keyPassphrase,
			TrustUnknownHost:   &trustUnknown,
			KnownHosts:         knownHosts,
			DialTimeoutSeconds: dialTimeout,
			Proxy:              proxy,
		},
	}

	// Optional single-level jump
	if jh := strings.TrimSpace(getString(args, "jump_host", "")); jh != "" {
		ju := strings.TrimSpace(getString(args, "jump_user", ""))
		jp := strings.TrimSpace(getString(args, "jump_password", ""))
		jpk := strings.TrimSpace(getString(args, "jump_private_key", ""))
		jkp := strings.TrimSpace(getString(args, "jump_key_passphrase", ""))
		jt := getBool(args, "jump_trust_unknown_host", false)
		jkh := strings.TrimSpace(getString(args, "jump_known_hosts", ""))
		jdt := int(getFloat64(args, "jump_dial_timeout_seconds", 30))
		jpx := strings.TrimSpace(getString(args, "jump_proxy", ""))
		jport := int(getFloat64(args, "jump_port", 22))
		entry.Jump = &sshconfig.JumpSpec{
			DialSpec: sshconfig.DialSpec{
				Host:               jh,
				Port:               jport,
				User:               ju,
				Password:           jp,
				PrivateKey:         jpk,
				KeyPassphrase:      jkp,
				TrustUnknownHost:   &jt,
				KnownHosts:         jkh,
				DialTimeoutSeconds: jdt,
				Proxy:              jpx,
			},
		}
	}

	// Refuse to overwrite an existing profile — use edit_ssh_config or copy_ssh_config.
	if names, err := s.sshConfigs.List(); err == nil {
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return toolError(CodeConflict, "%s", fmt.Sprintf("ssh config %q already exists (use edit_ssh_config or copy_ssh_config)", name)), nil
			}
		}
	}

	body, err := toml.Marshal(entry)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		// A body that fails validation is the caller's argument, not a server
		// fault: this used to answer 500 operation_failed for a profile missing its
		// host or credential, which reads like an outage and invites a retry that
		// cannot succeed.
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleDeleteSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	name := strings.TrimSpace(getString(request.GetArguments(), "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	// CHECK IT EXISTS FIRST, because Store.Delete deliberately ignores a missing
	// file and the reply must not inherit that.
	//
	// Delete is idempotent ON PURPOSE — an internal cleanup that runs twice should not
	// fail — but a caller asking to delete `prod` needs to know whether `prod` was
	// there. Answering 200 for a name that never existed makes a typo look like a
	// successful removal, and it is the only delete in this system that behaves that
	// way: `ext delete`, `rm <session>` and `fwd --action close` all answer 404.
	//
	// The check is Load, not List: Load is the same door every other read uses, so a
	// profile that cannot be loaded is not reported as removed either.
	if _, err := s.sshConfigs.Load(name); err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Delete(name); err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleCopySSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	src := strings.TrimSpace(getString(args, "source_name", ""))
	dst := strings.TrimSpace(getString(args, "target_name", ""))
	if src == "" || dst == "" {
		return toolError(CodeInvalidArgument, "%s", "source_name and target_name are required"), nil
	}
	data, err := s.sshConfigs.ReadRaw(src)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(dst, data); err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleEditSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}

	existing, err := s.sshConfigs.Load(name)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}

	applyEntryEdits(existing, args)

	applyJumpEdits(existing, args)

	body, err := toml.Marshal(existing)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		// A body that fails validation is the caller's argument, not a server
		// fault: this used to answer 500 operation_failed for a profile missing its
		// host or credential, which reads like an outage and invites a retry that
		// cannot succeed.
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleListSSHConfigs(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return jsonResult(map[string]any{"ssh_configs": []any{}}), nil
	}
	names, err := s.sshConfigs.List()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// A LISTING THAT CANNOT TELL A USABLE HOST FROM A BROKEN ONE IS A LISTING THAT
	// LIES. Every entry used to be printed as a bare name whether or not it parsed,
	// so the only way to learn that a profile was unusable was to try connecting to
	// it — and an inventory, a demo, or a runbook built from `ls host` claimed hosts
	// that could never be reached.
	//
	// The config is read at startup anyway, so the validation is free: the entry is
	// loaded here and the reason travels with it. The name is still listed rather
	// than hidden, because an entry the user wrote and cannot see is worse than one
	// they can see is broken.
	arr := make([]any, 0, len(names))
	for _, n := range names {
		if p, restricted, e := auth.PolicyFromContext(ctx); restricted && (e != nil || !s.agentHostAllowed(p, n)) {
			continue
		}
		if s.NoInternal && strings.EqualFold(n, "internal") {
			continue
		}
		item := map[string]any{"name": n}
		ent, err := s.sshConfigs.Load(n)
		if err != nil {
			item["broken"] = brokenReason(err)
		} else {
			// WHERE IT IS AND WHAT IT IS. A name alone cannot answer the first
			// question a newcomer has — `internal` sits in the same column as a box in
			// another rack, with nothing saying one is this machine and the other is
			// ssh. Deciding which is which by trying `uname -a` costs three calls and
			// is the wrong first impression to charge for a listing that already had
			// the answer loaded.
			//
			// A remote entry reports its TARGET rather than an OS: the OS of a host
			// that has never been connected to is unknown here, and a column that
			// guessed would be the kind of listing that lies.
			if ent.Kind == sshconfig.KindInternal {
				item["kind"] = "local"
				item["where"] = localWhere(ent)
			} else {
				item["kind"] = "ssh"
				item["where"] = targetOf(ent.DialSpec)
			}
		}
		arr = append(arr, item)
	}
	return jsonResult(map[string]any{"ssh_configs": arr}), nil
}

// localWhere names the OS and the shell a local session will run.
//
// The shell belongs in the first screen because it decides whether the caller's
// first command works: a caller that assumed bash typed `ls -la` into PowerShell and
// got `Get-ChildItem: 找不到与参数名称 'la' 匹配的参数`, with a PATH-supplied `uname`
// answering `MINGW64_NT` and confirming the wrong guess. What the profile names wins;
// otherwise this reports the shell the server would spawn, which is a fact only the
// server has (pwsh vs powershell vs cmd depends on PATH).
//
// The program name is read with strings.Fields rather than the argv grammar because
// this is a LABEL, not something to run: a quoted `"C:\Program Files\pwsh.exe" -NoLogo`
// may label as `"C:\Program` and still name the right shell to a human, while the
// grammar's error would leave the column empty.
func localWhere(ent *sshconfig.Entry) string {
	where := runtime.GOOS
	prog := ""
	if line := sshconfig.EffectiveDefaultShell(ent); line != "" {
		if f := strings.Fields(line); len(f) > 0 {
			prog = f[0]
		}
	} else {
		prog, _ = shell.NewDetector().Argv()
	}
	if prog != "" {
		where += " " + filepath.Base(prog)
	}
	return where
}

// targetOf renders the `user@host:port` a remote entry dials, for a listing column.
//
// An entry may leave any of the three out (the SSH defaults fill them in at dial
// time), so each part is printed only when it was written down: a fabricated
// `root@localhost:22` beside an entry that says nothing would be this listing
// inventing configuration.
func targetOf(d sshconfig.DialSpec) string {
	s := d.Host
	if d.Port > 0 {
		s += fmt.Sprintf(":%d", d.Port)
	}
	if d.User != "" {
		if s == "" {
			s = d.User + "@"
		} else {
			s = d.User + "@" + s
		}
	}
	return s
}

// brokenReason renders a load failure as one line fit for a listing column.
//
// The sentinel that told the surface which STATUS to answer is stripped: it is the
// tail of a Go error chain ("...: invalid ssh config"), a detail about how the
// failure is classified, not part of the sentence the operator has to act on.
func brokenReason(err error) string {
	msg := strings.TrimSpace(err.Error())
	for _, sentinel := range []error{sshconfig.ErrInvalid, sshconfig.ErrNotFound} {
		// Wrapped errors put the sentinel last ("...: invalid ssh config"), but a
		// returned one puts it first, so both ends are trimmed.
		msg = strings.TrimSuffix(msg, ": "+sentinel.Error())
		msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
	}
	return strings.Join(strings.Fields(msg), " ")
}

// handleGetSSHConfig returns one entry, with the secrets REDACTED.
//
// `ls host` only ever answered names, and nothing in the surface could answer "what is
// actually in this entry?" — so a caller editing a host had to guess at its current
// values, which is how a partial edit turns into a silent overwrite.
//
// Reading is not a mutation, so it is not behind --mcp-manage-ssh-configs: the list is
// already visible without it, and hiding the fields while showing the name protects
// nothing.
//
// THE SECRETS DO NOT COME BACK. A password or key passphrase returned here would end up
// in a transcript, a log line, or an assistant's context, and the caller's actual
// question — "is a password set?" — is answered by a boolean.
func (s *Server) handleGetSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeOperationFailed, "%s", "ssh config store not configured"), nil
	}
	name := strings.TrimSpace(getString(request.GetArguments(), "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	ent, err := s.sshConfigs.Load(name)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			// THE CODE NAMES THE THING THAT IS MISSING. It was session_not_found, which
			// sent a caller looking for a session: the answer to `get host prod` said
			// "ssh config not found" in the body and "no such session" in the header, and
			// the two disagree about which collection was searched. ssh_config_not_found
			// already existed and is what sshConfigErrCode answers for the same store.
			return toolError(CodeSSHConfigNotFound, "%s", "ssh config \""+name+"\" not found"), nil
		}
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}

	dial := func(d sshconfig.DialSpec) map[string]any {
		m := map[string]any{
			"host":           d.Host,
			"port":           d.Port,
			"user":           d.User,
			"has_password":   d.Password != "",
			"private_key":    d.PrivateKey,
			"known_hosts":    d.KnownHosts,
			"proxy":          d.Proxy,
			"dial_timeout":   d.DialTimeoutSeconds,
			"has_passphrase": d.KeyPassphrase != "",
		}
		if d.TrustUnknownHost != nil {
			m["trust_unknown_host"] = *d.TrustUnknownHost
		}
		if _, restricted, _ := auth.PolicyFromContext(ctx); restricted {
			delete(m, "private_key")
			delete(m, "proxy")
			m["has_private_key"] = d.PrivateKey != ""
			m["has_proxy"] = d.Proxy != ""
		}
		return m
	}

	out := map[string]any{
		"name":        name,
		"kind":        ent.Kind,
		"description": ent.Description,
	}
	for k, v := range dial(ent.DialSpec) {
		out[k] = v
	}
	if ent.DefaultShell != "" {
		if _, restricted, _ := auth.PolicyFromContext(ctx); restricted {
			out["has_default_shell"] = true
		} else {
			out["default_shell"] = ent.DefaultShell
		}
	}
	if ent.DefaultMode != "" {
		out["default_mode"] = ent.DefaultMode
	}
	if ent.DefaultApproval {
		out["default_approval"] = true
	}
	if ent.Jump != nil {
		out["jump"] = dial(ent.Jump.DialSpec)
	}
	return jsonResult(out), nil
}

func (s *Server) handleDetectShell(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	path, family, hint := shell.NewDetector().Detect()
	if path == "" {
		return toolError(CodeOperationFailed, "%s", hint), nil
	}
	result := map[string]any{
		"path":   path,
		"family": family,
		"hint":   hint,
	}
	return jsonResult(result), nil
}

// --- Port forwarding tool handlers ---

// applyEntryEdits overlays the arguments a caller supplied onto an existing
// profile. Only non-empty values overwrite: the edit form sends the fields the
// operator actually changed, and requiring a value to be present is what keeps
// an unrelated edit from clearing a setting nobody mentioned. The two boolean
// fields are the exception - they are decided by presence, see each case.
func applyEntryEdits(existing *sshconfig.Entry, args map[string]any) {
	// Merge: apply non-empty values from args over existing entry.
	if v := getString(args, "host", ""); v != "" {
		existing.Host = strings.TrimSpace(v)
	}
	if v := getString(args, "user", ""); v != "" {
		existing.User = strings.TrimSpace(v)
	}
	if v := getString(args, "password", ""); v != "" {
		existing.Password = strings.TrimSpace(v)
	}
	if v := getString(args, "private_key", ""); v != "" {
		existing.PrivateKey = strings.TrimSpace(v)
	}
	if v := getString(args, "key_passphrase", ""); v != "" {
		existing.KeyPassphrase = strings.TrimSpace(v)
	}
	if v := getString(args, "description", ""); v != "" {
		existing.Description = strings.TrimSpace(v)
	}
	if v := getString(args, "default_shell", ""); v != "" {
		existing.DefaultShell = strings.TrimSpace(v)
	}
	if v := getString(args, "default_mode", ""); v != "" {
		existing.DefaultMode = strings.TrimSpace(v)
	}
	// Presence, not truth: sending `default_approval: false` is how a profile is
	// turned back off. Requiring the key to be present is what keeps an unrelated
	// edit from clearing a setting the caller never mentioned.
	if _, ok := args["default_approval"]; ok {
		existing.DefaultApproval = getBool(args, "default_approval", false)
	}
	if v := getString(args, "known_hosts", ""); v != "" {
		existing.KnownHosts = strings.TrimSpace(v)
	}
	if v := getString(args, "proxy", ""); v != "" {
		existing.Proxy = strings.TrimSpace(v)
	}
	// Presence, not a sentinel: an edit updates only what the caller named.
	// Matches the `if _, ok := args["trust_unknown_host"]` right below it.
	if v := intPtrFromArgs(args, "port"); v != nil && *v > 0 {
		existing.Port = *v
	}
	if v := intPtrFromArgs(args, "dial_timeout_seconds"); v != nil && *v >= 0 {
		existing.DialTimeoutSeconds = *v
	}
	if _, ok := args["trust_unknown_host"]; ok {
		t := getBool(args, "trust_unknown_host", false)
		existing.TrustUnknownHost = &t
	}
}

// applyJumpEdits overlays a bastion (ProxyJump) onto an existing profile.
//
// CREATING A BASTION NEEDS ITS HOST; EDITING ONE DOES NOT. That distinction was
// missing, and it made the rule "a jump is all-or-nothing" swallow real edits:
// `edit host prod --jump-port 2201` on a profile that already has a bastion answered
// 200 success and changed nothing, because the merge only ran when `jump_host` was
// given again. The caller had to restate the host to change a port — and nothing in
// the reply said so.
//
// So the remaining jump_* arguments apply when there is ALREADY a bastion to apply
// them to. A stray `--jump-user` on a profile with no jump still does not conjure one:
// that half of the original rule is what keeps a typo from inventing infrastructure.
func applyJumpEdits(existing *sshconfig.Entry, args map[string]any) {
	if jh := getString(args, "jump_host", ""); jh != "" {
		if existing.Jump == nil {
			existing.Jump = &sshconfig.JumpSpec{}
		}
		existing.Jump.Host = strings.TrimSpace(jh)
	}
	if existing.Jump == nil {
		return
	}
	{
		if v := getString(args, "jump_user", ""); v != "" {
			existing.Jump.User = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_password", ""); v != "" {
			existing.Jump.Password = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_private_key", ""); v != "" {
			existing.Jump.PrivateKey = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_key_passphrase", ""); v != "" {
			existing.Jump.KeyPassphrase = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_known_hosts", ""); v != "" {
			existing.Jump.KnownHosts = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_proxy", ""); v != "" {
			existing.Jump.Proxy = strings.TrimSpace(v)
		}
		if v := intPtrFromArgs(args, "jump_port"); v != nil && *v > 0 {
			existing.Jump.Port = *v
		}
		if v := intPtrFromArgs(args, "jump_dial_timeout_seconds"); v != nil && *v >= 0 {
			existing.Jump.DialTimeoutSeconds = *v
		}
		if _, ok := args["jump_trust_unknown_host"]; ok {
			t := getBool(args, "jump_trust_unknown_host", false)
			existing.Jump.TrustUnknownHost = &t
		}
	}
}
