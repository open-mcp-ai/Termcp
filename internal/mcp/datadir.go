package mcp

// SetDataDir sets the directory this server was configured with.
//
// IT POWERS A FIXED REFUSAL, not a configurable one: <dataDir>/access holds the
// root key local callers sign tokens with, and protected.go refuses every path
// argument that reaches into it before any whitelist is consulted.
func (s *Server) SetDataDir(dir string) {
	if s != nil {
		s.dataDir = dir
	}
}
