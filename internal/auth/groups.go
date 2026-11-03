package auth

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

const managedStateVersion = 3
const maxPermissionGroups = 512
const deniedGroupID = "system-deny"

type permissionGroup struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Description    string      `json:"description"`
	Enabled        bool        `json:"enabled"`
	Revision       uint64      `json:"revision"`
	CreatedAt      time.Time   `json:"created_at"`
	Policy         AgentPolicy `json:"policy"`
	PolicyWarnings []string    `json:"policy_warnings,omitempty"`
	PresetID       string      `json:"preset_id,omitempty"`
	PresetVersion  int         `json:"preset_version,omitempty"`
}

type groupPreset struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	English     string      `json:"english"`
	Description string      `json:"description"`
	Version     int         `json:"version"`
	Policy      AgentPolicy `json:"policy"`
}

func groupPresets() []groupPreset {
	viewer := []string{"profile.list", "profile.get", "session.list", "session.info", "shell.list", "ext.list", "task.list", "task.info"}
	diagnostics := append(slices.Clone(viewer), "shell.output", "shell.marks", "shell.detect", "file.read", "file.stat", "file.realpath", "file.readlink", "file.getwd", "file.statvfs", "file.urls")
	maintainer := append(slices.Clone(diagnostics), "file.write", "file.replace", "file.mkdir")
	operator := append(slices.Clone(maintainer),
		"session.start", "session.terminate",
		"shell.open", "shell.close", "shell.exec", "shell.input", "shell.key", "shell.resize",
		"shell.reader.register", "shell.reader.unregister",
		"file.delete", "file.truncate", "file.chmod", "file.chown", "file.chtimes",
		"file.symlink", "file.link", "file.rename",
		"forward.list", "forward.open.local", "forward.open.remote", "forward.open.dynamic", "forward.close",
		"task.cancel", "ext.get", "ext.use", "ext.release", "ext.retry", "user.notify",
		"alias.list",
	)
	out := []groupPreset{}
	for _, spec := range []struct {
		id, name, english, description string
		operations                     []string
	}{
		{"no-access", "未授权", "No access", "业务动作默认禁止；主机范围本身不授予操作权限。", nil},
		{"viewer", "资源观察", "Viewer", "查看安全摘要和状态，不读取终端输出或文件正文。", viewer},
		{"diagnostics", "只读诊断", "Diagnostics reader", "读取已分配资源的输出和文件内容，需要已有的可达会话。", diagnostics},
		{"file-maintainer", "文件维护", "File maintainer", "允许文件读写和目录维护；不授予终端执行、网络转发或全局配置管理。", maintainer},
		{"host-operator", "主机运维", "Host operator", "允许连接主机、操作终端、常用文件、转发和已配置扩展；不授予凭据配置、别名管理或 raw 协议。", operator},
	} {
		policy := EmptyAgentPolicy()
		for _, id := range spec.operations {
			policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
		}
		if spec.id == "diagnostics" {
			for _, id := range []string{"file.read", "file.write", "file.replace"} {
				if _, exists := policy.Actions[id]; exists {
					policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
				}
			}
		}
		if spec.id == "file-maintainer" {
			for _, id := range []string{"file.read", "file.write", "file.replace"} {
				policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
			}
		}
		if spec.id == "host-operator" {
			for _, id := range []string{"file.read", "file.write"} {
				policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline", "local", "pipe"}}}
			}
			policy.Actions["file.replace"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
		}
		out = append(out, groupPreset{spec.id, spec.name, spec.english, spec.description, 3, policy})
	}
	return out
}

// groupPresetsV1 is the first released built-in policy set. A server restart
// used to keep those seeded groups forever, so correcting a preset never
// reached an existing installation. Upgrade a built-in group only while its
// policy still exactly matches the shipped v1 template; edited groups remain
// under the administrator's control.
func groupPresetsV1() map[string]groupPreset {
	viewer := []string{"profile.list", "profile.get", "session.list", "session.info", "shell.list", "ext.list", "task.list", "task.info"}
	diagnostics := append(slices.Clone(viewer), "shell.output", "shell.marks", "file.read", "file.stat", "file.realpath", "file.readlink", "file.getwd", "file.statvfs")
	maintainer := append(slices.Clone(diagnostics), "file.write", "file.replace", "file.mkdir")
	operator := append(slices.Clone(maintainer), "session.start", "session.terminate", "shell.open", "shell.close", "shell.exec", "shell.input", "shell.key", "shell.resize", "shell.reader.register", "shell.reader.unregister", "user.notify")
	out := map[string]groupPreset{}
	for _, spec := range []struct {
		id, name, english, description string
		operations                     []string
	}{
		{"no-access", "未授权", "No access", "业务动作默认禁止；主机范围本身不授予操作权限。", nil},
		{"viewer", "资源观察", "Viewer", "查看安全摘要和状态，不读取终端输出或文件正文。", viewer},
		{"diagnostics", "只读诊断", "Diagnostics reader", "读取已分配资源的输出和文件内容，需要已有的可达会话。", diagnostics},
		{"file-maintainer", "文件维护", "File maintainer", "允许内联内容维护；修改被其他程序执行的文件仍可能产生后续影响。", maintainer},
		{"host-operator", "主机运维", "Host operator", "拥有目标 OS 账号的终端执行能力；关闭独立删除动作不能阻止 shell rm。", operator},
	} {
		policy := EmptyAgentPolicy()
		for _, id := range spec.operations {
			policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
		}
		if spec.id == "file-maintainer" || spec.id == "host-operator" {
			for _, id := range []string{"file.read", "file.write", "file.replace"} {
				policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
			}
		}
		out[spec.id] = groupPreset{ID: spec.id, Name: spec.name, English: spec.english, Description: spec.description, Version: 1, Policy: policy}
	}
	return out
}

// groupPresetsV2 is the corrected operation/risk set before terminal operators
// gained read access to their shared alias vocabulary.
func groupPresetsV2() map[string]groupPreset {
	viewer := []string{"profile.list", "profile.get", "session.list", "session.info", "shell.list", "ext.list", "task.list", "task.info"}
	diagnostics := append(slices.Clone(viewer), "shell.output", "shell.marks", "shell.detect", "file.read", "file.stat", "file.realpath", "file.readlink", "file.getwd", "file.statvfs", "file.urls")
	maintainer := append(slices.Clone(diagnostics), "file.write", "file.replace", "file.mkdir")
	operator := append(slices.Clone(maintainer),
		"session.start", "session.terminate",
		"shell.open", "shell.close", "shell.exec", "shell.input", "shell.key", "shell.resize",
		"shell.reader.register", "shell.reader.unregister",
		"file.delete", "file.truncate", "file.chmod", "file.chown", "file.chtimes",
		"file.symlink", "file.link", "file.rename",
		"forward.list", "forward.open.local", "forward.open.remote", "forward.open.dynamic", "forward.close",
		"task.cancel", "ext.get", "ext.use", "ext.release", "ext.retry", "user.notify",
	)
	out := map[string]groupPreset{}
	for _, spec := range []struct {
		id, name, english, description string
		operations                     []string
	}{
		{"no-access", "未授权", "No access", "业务动作默认禁止；主机范围本身不授予操作权限。", nil},
		{"viewer", "资源观察", "Viewer", "查看安全摘要和状态，不读取终端输出或文件正文。", viewer},
		{"diagnostics", "只读诊断", "Diagnostics reader", "读取已分配资源的输出和文件内容，需要已有的可达会话。", diagnostics},
		{"file-maintainer", "文件维护", "File maintainer", "允许文件读写和目录维护；不授予终端执行、网络转发或全局配置管理。", maintainer},
		{"host-operator", "主机运维", "Host operator", "允许连接主机、操作终端、常用文件、转发和已配置扩展；不授予凭据配置、公共别名或 raw 协议。", operator},
	} {
		policy := EmptyAgentPolicy()
		for _, id := range spec.operations {
			policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
		}
		if spec.id == "diagnostics" {
			for _, id := range []string{"file.read", "file.write", "file.replace"} {
				if _, exists := policy.Actions[id]; exists {
					policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
				}
			}
		}
		if spec.id == "file-maintainer" {
			for _, id := range []string{"file.read", "file.write", "file.replace"} {
				policy.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
			}
		}
		if spec.id == "host-operator" {
			policy.Actions["file.read"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline", "local", "pipe"}}}
			policy.Actions["file.write"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline", "local", "pipe"}}}
			policy.Actions["file.replace"] = authorization.Grant{Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{Forms: []string{"inline"}}}
		}
		out[spec.id] = groupPreset{ID: spec.id, Name: spec.name, English: spec.english, Description: spec.description, Version: 2, Policy: policy}
	}
	return out
}

func upgradePresetGroups(s *managedState) bool {
	current := map[string]groupPreset{}
	for _, preset := range groupPresets() {
		current[preset.ID] = preset
	}
	history := map[int]map[string]groupPreset{
		1: groupPresetsV1(),
		2: groupPresetsV2(),
	}
	changed := false
	for id, group := range s.Groups {
		preset, ok := current[group.PresetID]
		if !ok || group.ID != "preset-"+preset.ID || group.PresetVersion <= 0 || group.PresetVersion >= preset.Version {
			continue
		}
		legacy, ok := history[group.PresetVersion][group.PresetID]
		if !ok || !reflect.DeepEqual(group.Policy, legacy.Policy) {
			continue
		}
		group.Policy = clonePolicy(preset.Policy)
		if group.Description == legacy.Description {
			group.Description = preset.Description
		}
		group.PresetVersion = preset.Version
		group.Revision++
		s.Groups[id] = group
		changed = true
	}
	if changed {
		s.AccessRevision++
	}
	return changed
}

func seedPermissionGroups(s *managedState, now time.Time) {
	s.Groups = map[string]permissionGroup{}
	for _, preset := range groupPresets() {
		id := "preset-" + preset.ID
		s.Groups[id] = permissionGroup{ID: id, Name: preset.Name, Description: preset.Description, Enabled: true, Revision: 1, CreatedAt: now, Policy: clonePolicy(preset.Policy), PresetID: preset.ID, PresetVersion: preset.Version}
	}
	s.DefaultGroupID = "preset-no-access"
	s.AccessRevision = 1
}

func groupNameAvailable(s managedState, name, except string) bool {
	for id, g := range s.Groups {
		if id != except && strings.EqualFold(g.Name, name) {
			return false
		}
	}
	return true
}

func boundGroupID(s managedState, a agentRecord) string {
	if a.GroupID != "" {
		return a.GroupID
	}
	return s.DefaultGroupID
}
func resolvedAgentPolicy(s managedState, a agentRecord) (AgentPolicy, error) {
	if s.Version != managedStateVersion {
		return AgentPolicy{}, errors.New("unsupported permission state version")
	}
	id := boundGroupID(s, a)
	if id == deniedGroupID {
		return AgentPolicy{}, errors.New("principal has no active permission group")
	}
	group, ok := s.Groups[id]
	if !ok || !group.Enabled {
		return AgentPolicy{}, errors.New("permission group is missing or disabled")
	}
	return clonePolicy(group.Policy), nil
}
func stateAgentPolicy(s managedState, id string) AgentPolicy {
	a, ok := s.Agents[id]
	if !ok {
		return EmptyAgentPolicy()
	}
	p, err := resolvedAgentPolicy(s, a)
	if err != nil {
		return EmptyAgentPolicy()
	}
	return p
}

func cloneGroups(in map[string]permissionGroup) map[string]permissionGroup {
	out := maps.Clone(in)
	if out == nil {
		out = map[string]permissionGroup{}
	}
	return out
}
func validateGroupState(s managedState) error {
	if len(s.Groups) > maxPermissionGroups || s.AccessRevision == 0 {
		return errors.New("invalid permission group state")
	}
	if _, ok := s.Groups[s.DefaultGroupID]; !ok {
		return errors.New("default permission group is missing")
	}
	names := map[string]bool{}
	for id, g := range s.Groups {
		if id == "" || id == deniedGroupID || id != g.ID || !validLabel(g.Name) || g.Revision == 0 || g.CreatedAt.IsZero() || len(g.Description) > 2048 {
			return errors.New("invalid permission group")
		}
		name := strings.ToLower(g.Name)
		if names[name] {
			return errors.New("duplicate permission group name")
		}
		names[name] = true
		if err := validateAgentPolicy(g.Policy); err != nil {
			return fmt.Errorf("group %s: %w", id, err)
		}
	}
	for _, a := range s.Agents {
		id := boundGroupID(s, a)
		if id == deniedGroupID {
			continue
		}
		if _, ok := s.Groups[id]; !ok {
			return errors.New("principal references a missing permission group")
		}
	}
	return nil
}

func groupMemberIDs(s managedState, groupID string) []string {
	ids := []string{}
	for id, a := range s.Agents {
		if boundGroupID(s, a) == groupID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *Managed) cancelPrincipalsLocked(ids []string) {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	for id, token := range m.state.Tokens {
		if wanted[token.PrincipalID] {
			m.cancelCredentialLocked(id)
		}
	}
}
