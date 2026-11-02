package mcp

// newAliasTableForTest builds a table from a literal map. Tests name the entries
// they care about; the table itself is a sync.Map, which a literal cannot express.
func newAliasTableForTest(entries map[string]string) *aliasTable {
	a := &aliasTable{}
	for k, v := range entries {
		a.set(k, v)
	}
	return a
}
