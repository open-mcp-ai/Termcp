package auth

import (
	"strings"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

// legacyOperations is a frozen expansion, not a family wildcard. New catalog
// entries must never become grants just because an old group was checked.
var legacyOperations = map[string]string{
	"session_read":   "session.list session.info shell.list task.list task.info",
	"session_manage": "session.start session.terminate session.delete shell.close",
	"terminal_read":  "shell.output shell.marks shell.detect shell.reader.register shell.reader.unregister",
	"terminal_write": "shell.open shell.exec shell.input shell.key shell.resize",
	"file_read":      "file.read file.stat file.urls file.getwd",
	"file_write":     "file.write file.replace file.delete file.rename file.mkdir file.chmod file.chown file.chtimes file.symlink file.link file.truncate task.cancel",
	"forward":        "forward.list forward.open.local forward.open.remote forward.open.dynamic forward.close",
	"peer":           "mcp.open mcp.list mcp.info mcp.close mcp.read mcp.tools.list mcp.tool.invoke mcp.raw cdp.open cdp.list cdp.info cdp.close cdp.read cdp.protocol.read cdp.attach cdp.detach cdp.method.invoke cdp.raw jrpc.open jrpc.list jrpc.info jrpc.close jrpc.read jrpc.method.invoke jrpc.raw",
	"profile_read":   "profile.list profile.get",
	"profile_write":  "profile.create profile.edit profile.copy profile.delete",
	"extension":      "ext.list ext.get ext.use ext.release ext.retry ext.create ext.update ext.delete",
	"alias":          "alias.list alias.write alias.delete",
	"notify":         "user.notify",
}

// conservativeUpgrade is a candidate, never an automatic migration. Ambiguous
// read-only fs/link branches require both old file groups before being selected.
func conservativeUpgrade(p AgentPolicy) AgentPolicy {
	out := AgentPolicy{SchemaVersion: 2, Functions: []string{}, HostMode: p.HostMode, Hosts: append([]string{}, p.Hosts...), Actions: map[string]authorization.Grant{}}
	for _, group := range p.Functions {
		for _, id := range strings.Fields(legacyOperations[group]) {
			op, _ := authorization.Lookup(id)
			if op.Global && p.HostMode != "all" {
				continue
			}
			out.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
		}
	}
	if p.Allows("file_read") && p.Allows("file_write") {
		for _, id := range []string{"file.realpath", "file.readlink", "file.statvfs"} {
			out.Actions[id] = authorization.Grant{Effect: authorization.EffectAllow}
		}
	}
	return out
}
