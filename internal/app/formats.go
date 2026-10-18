package app

import (
	"micastgo/internal/store"
	"time"
)

// Serving bytes is evidence of delivery, never proof of audible playback.
type FormatEvidence struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

func (a *App) RecordFormat(user, id, format string) {
	if !store.ValidFormat(format) || user == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tokens.UserID != user {
		return
	}
	if a.formats == nil {
		a.formats = map[string]map[string]FormatEvidence{}
	}
	key := user + "/" + id
	if a.formats[key] == nil {
		if len(a.formats) >= 512 {
			clear(a.formats)
		}
		a.formats[key] = map[string]FormatEvidence{}
	}
	a.formats[key][format] = FormatEvidence{"served", time.Now()}
}
