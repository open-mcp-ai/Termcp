package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

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
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
	if err := s.sshConfigs.Delete(name); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
	if v := getFloat64(args, "port", -1); v >= 0 {
		existing.Port = int(v)
	}
	if v := getFloat64(args, "dial_timeout_seconds", -1); v >= 0 {
		existing.DialTimeoutSeconds = int(v)
	}
	if _, ok := args["trust_unknown_host"]; ok {
		t := getBool(args, "trust_unknown_host", false)
		existing.TrustUnknownHost = &t
	}

	// Jump merge
	if jh := getString(args, "jump_host", ""); jh != "" {
		if existing.Jump == nil {
			existing.Jump = &sshconfig.JumpSpec{}
		}
		existing.Jump.Host = strings.TrimSpace(jh)
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
		if v := getFloat64(args, "jump_port", -1); v >= 0 {
			existing.Jump.Port = int(v)
		}
		if v := getFloat64(args, "jump_dial_timeout_seconds", -1); v >= 0 {
			existing.Jump.DialTimeoutSeconds = int(v)
		}
		if _, ok := args["jump_trust_unknown_host"]; ok {
			t := getBool(args, "jump_trust_unknown_host", false)
			existing.Jump.TrustUnknownHost = &t
		}
	}

	body, err := toml.Marshal(existing)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
	arr := make([]any, 0, len(names))
	for _, n := range names {
		if s.NoInternal && strings.EqualFold(n, "internal") {
			continue
		}
		arr = append(arr, n)
	}
	return jsonResult(map[string]any{"ssh_configs": arr}), nil
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
