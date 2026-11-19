package emulation

// newKey tire une key selon le motif du profil (e.mu tenu, e.p.Key non nil).
func (e *Emulator) newKey() string {
	return string(e.p.c.key.Generate(e.rng))
}
