package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
)

// AgentPolicy grants named function groups and stable saved-profile identities.
// Empty selections grant nothing; omitted policies on older Agents retain their
// previous access until an administrator explicitly saves a policy.
type AgentPolicy struct {
	Functions []string `json:"functions"`
	HostMode  string   `json:"host_mode"`
	Hosts     []string `json:"hosts"`
}

type FunctionGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	English     string `json:"english"`
	Description string `json:"description"`
}

var functionGroups = []FunctionGroup{
	{"session_read", "查看会话", "View sessions", "查看连接、通道状态与任务进度"},
	{"session_manage", "管理会话", "Manage sessions", "连接主机、关闭连接和删除会话"},
	{"terminal_read", "读取终端", "Read terminals", "读取输出、状态跨度和管理读取游标"},
	{"terminal_write", "操作终端", "Operate terminals", "运行命令、发送输入和按键、打开通道"},
	{"file_read", "读取文件", "Read files", "读取文件、目录、属性和下载"},
	{"file_write", "修改文件", "Modify files", "写入、上传、复制、删除和修改文件属性"},
	{"forward", "端口转发", "Port forwarding", "查看、创建和关闭端口转发"},
	{"peer", "外部协议", "External protocols", "连接和调用 MCP、CDP、JSON-RPC 服务"},
	{"profile_read", "查看主机配置", "View profiles", "查看已授权主机的配置与连接信息"},
	{"profile_write", "修改主机配置", "Modify profiles", "创建、编辑和删除主机配置；需要全部主机范围"},
	{"extension", "管理扩展", "Manage extensions", "查看、安装和使用扩展；需要全部主机范围"},
	{"alias", "管理别名", "Manage aliases", "查看和修改共享别名；展开后的操作仍需授权"},
	{"notify", "发送通知", "Send notifications", "向管理员发送界面通知"},
}

func FunctionGroups() []FunctionGroup { return slices.Clone(functionGroups) }

func FullAgentPolicy() AgentPolicy {
	p := AgentPolicy{HostMode: "all", Hosts: []string{}, Functions: []string{}}
	for _, g := range functionGroups {
		p.Functions = append(p.Functions, g.ID)
	}
	return p
}

func (p AgentPolicy) Allows(function string) bool { return slices.Contains(p.Functions, function) }
func (p AgentPolicy) AllowsHost(id string) bool {
	return id != "" && (p.HostMode == "all" || p.HostMode == "selected" && slices.Contains(p.Hosts, id))
}
func clonePolicy(p AgentPolicy) AgentPolicy {
	p.Functions = append([]string{}, p.Functions...)
	p.Hosts = append([]string{}, p.Hosts...)
	return p
}
func effectivePolicy(a agentRecord) AgentPolicy {
	if a.Policy == nil {
		return FullAgentPolicy()
	}
	return clonePolicy(*a.Policy)
}
func agentRevision(a agentRecord) uint64 {
	if a.Revision == 0 {
		return 1
	}
	return a.Revision
}

func validateAgentPolicy(p AgentPolicy) error {
	if p.HostMode != "all" && p.HostMode != "selected" && p.HostMode != "none" {
		return errors.New("host_mode must be all, selected or none")
	}
	if len(p.Functions) > len(functionGroups) || len(p.Hosts) > 1024 || p.HostMode != "selected" && len(p.Hosts) > 0 {
		return errors.New("invalid policy selection")
	}
	seen := map[string]bool{}
	for _, id := range p.Functions {
		known := false
		for _, g := range functionGroups {
			if g.ID == id {
				known = true
			}
		}
		if !known || seen[id] {
			return errors.New("unknown or duplicate function permission")
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range p.Hosts {
		if id == "" || len(id) > 128 || seen[id] {
			return errors.New("invalid or duplicate host identity")
		}
		seen[id] = true
	}
	if p.HostMode != "all" && (p.Allows("profile_write") || p.Allows("extension")) {
		return errors.New("profile changes and extension management require all hosts")
	}
	return nil
}

type agentPolicyKey struct{}
type policyResolver func() (AgentPolicy, error)

// PolicyFromContext consults live server state, never a client-provided policy.
func PolicyFromContext(ctx context.Context) (AgentPolicy, bool, error) {
	resolver, ok := ctx.Value(agentPolicyKey{}).(policyResolver)
	if !ok {
		return AgentPolicy{}, false, nil
	}
	if ctx.Err() != nil {
		return AgentPolicy{}, true, ctx.Err()
	}
	p, err := resolver()
	return p, true, err
}

func (m *Managed) withAgentPolicy(ctx context.Context, principal, credential string) context.Context {
	return context.WithValue(ctx, agentPolicyKey{}, policyResolver(func() (AgentPolicy, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		a, ok := m.state.Agents[principal]
		t, exists := m.state.Tokens[credential]
		permitted := slices.Contains(m.state.Permissions[RoleAgent], "mcp")
		if !ok || !a.Enabled || !exists || t.PrincipalID != principal || !permitted {
			return AgentPolicy{}, errors.New("Agent access has ended")
		}
		return effectivePolicy(a), nil
	}))
}

func (m *Managed) accessCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		authError(w, 405, "method not allowed")
		return
	}
	type host struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	hosts := []host{}
	if catalog, ok := m.vault.(interface {
		List() ([]string, error)
		Identity(string) (string, error)
	}); ok {
		names, err := catalog.List()
		if err != nil {
			authError(w, 500, "could not list saved hosts")
			return
		}
		for _, name := range names {
			if m.HideInternal && name == "internal" {
				continue
			}
			id, err := catalog.Identity(name)
			if err != nil {
				authError(w, 500, "could not resolve saved host identities")
				return
			}
			hosts = append(hosts, host{id, name})
		}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	authJSON(w, 200, map[string]any{"functions": FunctionGroups(), "hosts": hosts})
}
