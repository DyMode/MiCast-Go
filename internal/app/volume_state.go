package app

// Latest successful command or readback. Expiring an observation must not
// replace a known hardware level with a saved startup default.
func (a *App) rememberVolumeLocked(user, id string, value int) {
	if a.volumes == nil {
		a.volumes = map[string]int{}
	}
	if len(a.volumes) >= 512 {
		clear(a.volumes)
	}
	a.volumes[user+"/"+id] = value
}
