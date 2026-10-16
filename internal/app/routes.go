package app

import (
	"context"
	"errors"
	"sync"

	"micastgo/internal/store"
)

type routeState struct {
	source, audioID string
	paused          bool
	resolver        func(store.Speaker) string
	owned, blocked  map[string]bool
}

func (a *App) route(id string) *routeState {
	r := a.routes[id]
	if r == nil {
		r = &routeState{owned: map[string]bool{}, blocked: map[string]bool{}}
		a.routes[id] = r
	}
	return r
}
func (r *routeState) url(s store.Speaker) string {
	if r.resolver != nil {
		return r.resolver(s)
	}
	return r.source
}

// claim prevents an older route from recovering a speaker taken by a newer cast.
// All route state is ordered by App.op, including configuration reconciliation.
func (a *App) claim(route, id string) {
	if old, ok := a.owner(id); ok && old != route {
		previous := a.route(old)
		delete(previous.owned, id)
		previous.blocked[id] = true
	}
	a.ownerMu.Lock()
	a.owners[id] = route
	a.ownerMu.Unlock()
	a.route(route).owned[id] = true
}
func (a *App) Control(ctx context.Context, action, raw, audioID string, volume int) (map[string]string, error) {
	return a.ControlRoute(ctx, "", action, raw, audioID, volume)
}
func (a *App) ControlRoute(ctx context.Context, route, action, raw, audioID string, volume int) (map[string]string, error) {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.route(route)
	if action == "play_url" {
		r.resolver = nil
	}
	return a.controlRoute(ctx, route, action, raw, audioID, volume, action == "play_url")
}

// ControlRouteSource also protects a newly recreated endpoint from commands
// arriving through an old engine reference during enable/disable transitions.
func (a *App) ControlRouteSource(ctx context.Context, route, source, action string, volume int) (map[string]string, error) {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.routes[route]
	if r == nil || r.source != source {
		return map[string]string{}, nil
	}
	return a.controlRoute(ctx, route, action, "", "", volume, false)
}
func (a *App) BeginStream(ctx context.Context, source, audioID string, resolver func(store.Speaker) string) (map[string]string, error) {
	return a.BeginRouteStream(ctx, "", source, audioID, resolver)
}
func (a *App) BeginRouteStream(ctx context.Context, route, source, audioID string, resolver func(store.Speaker) string, continuation ...bool) (map[string]string, error) {
	a.op.Lock()
	defer a.op.Unlock()
	a.route(route).resolver = resolver
	fresh := len(continuation) == 0 || !continuation[0]
	return a.controlRoute(ctx, route, "play_url", source, audioID, 0, fresh)
}
func (a *App) controlRoute(ctx context.Context, route, action, raw, audioID string, volume int, fresh bool) (map[string]string, error) {
	a.mu.RLock()
	tokens, status := a.tokens, a.status
	a.mu.RUnlock()
	if tokens.UserID == "" || status == "expired" {
		return nil, errors.New("请先连接米家账号")
	}
	r := a.route(route)
	targets := a.config.Snapshot().RouteTargets(route)
	if fresh && len(targets) == 0 {
		return nil, errors.New("当前入口没有启用的音箱")
	}
	switch action {
	case "play_url":
		r.source = raw
		r.audioID = audioID
		r.paused = false
	case "pause":
		r.paused = true
	case "play":
		r.paused = false
	case "stop":
		r.source = ""
		r.paused = false
	}
	if fresh {
		r.blocked = map[string]bool{}
		for _, s := range targets {
			a.claim(route, s.ID)
		}
	}
	if action == "play_url" && !fresh {
		for _, s := range targets {
			owner, occupied := a.owner(s.ID)
			if !r.blocked[s.ID] && (!occupied || owner == route) {
				a.claim(route, s.ID)
			}
		}
	}
	results := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	gate := make(chan struct{}, 4)
	for _, s := range targets {
		// Transport controls and metadata may only affect this route's speakers.
		if !r.owned[s.ID] || r.blocked[s.ID] {
			continue
		}
		wg.Add(1)
		go func(s store.Speaker) {
			defer wg.Done()
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				mu.Lock()
				results[s.ID] = ctx.Err().Error()
				mu.Unlock()
				return
			}
			var err error
			state := "command_accepted"
			switch action {
			case "play_url":
				err = a.cloud.PlayURL(ctx, tokens, s.ID, s.Hardware, r.url(s), r.audioID)
			case "volume":
				err = a.cloud.Command(ctx, tokens, s.ID, "player_set_volume", map[string]any{"media": "app_ios", "volume": volume})
				state = ""
			default:
				err = a.cloud.Command(ctx, tokens, s.ID, "player_play_operation", map[string]any{"media": "app_ios", "action": action})
				if action == "pause" {
					state = "paused"
				}
				if action == "stop" {
					state = "stopped"
				}
			}
			a.recordCommand(s.ID, state, err)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[s.ID] = err.Error()
			} else {
				results[s.ID] = "accepted"
			}
		}(s)
	}
	wg.Wait()
	if action == "stop" {
		for id := range r.owned {
			a.release(route, id)
		}
		r.owned = map[string]bool{}
	}
	return results, nil
}
func (a *App) RetryTargets(ctx context.Context, source string, ids map[string]bool) error {
	return a.RetryRouteTargets(ctx, "", source, ids)
}
func (a *App) RetryRouteTargets(ctx context.Context, route, source string, ids map[string]bool) error {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.route(route)
	if r.source != source || r.paused {
		return nil
	}
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	var failures []error
	for _, s := range a.config.Snapshot().RouteTargets(route) {
		if !ids[s.ID] || !r.owned[s.ID] || r.blocked[s.ID] {
			continue
		}
		err := a.cloud.PlayURL(ctx, tokens, s.ID, s.Hardware, r.url(s), r.audioID)
		a.recordCommand(s.ID, "command_accepted", err)
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
func (a *App) RefreshDisplay(ctx context.Context, audioID string) error {
	return a.RefreshRouteDisplay(ctx, "", audioID)
}
func (a *App) RefreshRouteDisplay(ctx context.Context, route, audioID string, source ...string) error {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.route(route)
	if len(source) > 0 && r.source != source[0] {
		return nil
	}
	if r.source == "" || r.paused {
		return nil
	}
	_, err := a.controlRoute(ctx, route, "play_url", r.source, audioID, 0, false)
	return err
}
func (a *App) reconcile(ctx context.Context, before, after store.Config) error {
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	var failures []error
	for route, r := range a.routes {
		targets, old := map[string]store.Speaker{}, map[string]store.Speaker{}
		for _, s := range after.RouteTargets(route) {
			targets[s.ID] = s
		}
		for _, s := range before.RouteTargets(route) {
			old[s.ID] = s
		}
		for id := range r.owned {
			if _, keep := targets[id]; keep {
				continue
			}
			err := a.cloud.Command(ctx, tokens, id, "player_play_operation", map[string]any{"media": "app_ios", "action": "stop"})
			a.recordCommand(id, "stopped", err)
			failures = append(failures, err)
			// Withdraw ownership even if offline: an obsolete route must never retry it.
			delete(r.owned, id)
			a.release(route, id)
		}
		if r.source == "" || r.paused || tokens.UserID == "" {
			continue
		}
		for id, s := range targets {
			if r.blocked[id] {
				continue
			}
			if owner, occupied := a.owner(id); occupied && owner != route {
				continue
			}
			if previous, exists := old[id]; exists && previous.Format == s.Format {
				continue
			}
			a.claim(route, id)
			err := a.cloud.PlayURL(ctx, tokens, id, s.Hardware, r.url(s), r.audioID)
			a.recordCommand(id, "command_accepted", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// RouteOwns gates existing HTTP pulls after another entry takes this speaker.
func (a *App) RouteOwns(route, id string) bool { owner, ok := a.owner(id); return ok && owner == route }
func (a *App) owner(id string) (string, bool) {
	a.ownerMu.RLock()
	defer a.ownerMu.RUnlock()
	owner, ok := a.owners[id]
	return owner, ok
}
func (a *App) release(route, id string) {
	a.ownerMu.Lock()
	defer a.ownerMu.Unlock()
	if a.owners[id] == route {
		delete(a.owners, id)
	}
}

func (a *App) RouteMembers(route string) []string {
	a.ownerMu.RLock()
	defer a.ownerMu.RUnlock()
	result := []string{}
	for id, owner := range a.owners {
		if owner == route {
			result = append(result, id)
		}
	}
	return result
}
