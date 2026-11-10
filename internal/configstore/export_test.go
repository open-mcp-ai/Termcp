package configstore

// Test-only accessors so tests do not poke at private snapshot fields.

func (r *Repository) refsSnapshot() map[string]catalogRef {
	return cloneRefs(r.snap().refs)
}

func (r *Repository) revisionValue() uint64 {
	return r.snap().revision
}

func (r *Repository) catalogGenerationID() ID {
	return r.snap().catalogGeneration
}

func (r *Repository) keyringForTest() *Keyring {
	return r.snap().keyring
}
