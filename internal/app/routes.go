package app

import (
	"context"
	"errors"

	"micastgo/internal/store"
)

type StreamOptions struct {
	Background func() bool
	Volume     func() (int, bool) // Optional hardware volume for this source.
}

type routeState struct {
	continuation    bool
	volume          func() (int, bool)
	source, audioID string
	paused          bool
	resolver        func(store.Speaker) string
	owned, blocked  map[string]bool
	revision        uint64
	intentPaused    bool
	started         map[string]bool
	live            func() bool
	onLost          func()
	background      func() bool
}

func (a *App) route(id string) *routeState {
	r := a.routes[id]
	if r == nil {
		r = &routeState{owned: map[string]bool{}, blocked: map[string]bool{}, started: map[string]bool{}}
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
		delete(previous.started, id)
		previous.blocked[id] = true
		if len(previous.owned) == 0 {
			if previous.onLost != nil {
				previous.onLost()
			}
			previous.source, previous.audioID, previous.resolver = "", "", nil
			previous.live, previous.background, previous.onLost = nil, nil, nil
			previous.volume = nil
			previous.revision++
		}
	}
	if a.leases == nil {
		a.leases = map[string]*targetLease{}
	}
	if old := a.leases[id]; old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.leases[id] = &targetLease{ctx: ctx, cancel: cancel}
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
		r.live, r.onLost, r.background = nil, nil, nil
		r.volume = nil
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
	return a.BeginGuardedRouteStream(ctx, route, source, audioID, resolver, len(continuation) > 0 && continuation[0], nil, nil)
}

func (a *App) BeginGuardedRouteStream(ctx context.Context, route, source, audioID string, resolver func(store.Speaker) string, continuation bool, live func() bool, onLost func(), options ...StreamOptions) (map[string]string, error) {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.route(route)
	if ctx.Err() != nil || (live != nil && !live()) {
		return nil, ErrSuperseded
	}
	r.continuation = continuation
	r.resolver, r.live, r.onLost = resolver, live, onLost
	r.background = live
	r.volume = nil
	if len(options) > 0 {
		if options[0].Background != nil {
			r.background = options[0].Background
		}
		r.volume = options[0].Volume
	}
	return a.controlRoute(ctx, route, "play_url", source, audioID, 0, !continuation)
}
func (a *App) controlRoute(ctx context.Context, route, action, raw, audioID string, volume int, fresh bool) (map[string]string, error) {
	a.mu.RLock()
	tokens, status := a.tokens, a.status
	a.mu.RUnlock()
	if (tokens.UserID == "" || status == "expired") && action != "stop" {
		return nil, errors.New("请先连接米家账号")
	}
	r := a.route(route)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	targets := a.config.Snapshot().RouteTargets(route)
	if fresh && len(targets) == 0 {
		return nil, errors.New("当前入口没有启用的音箱")
	}
	if action == "volume" && (r.paused || r.intentPaused) {
		a.mu.Lock()
		for id := range r.owned {
			a.rememberVolumeLocked(tokens.UserID, id, volume)
		}
		a.mu.Unlock()
	}
	switch action {
	case "play_url":
		if r.live != nil && !r.live() {
			return nil, ErrSuperseded
		}
		r.revision++
		r.source = raw
		r.audioID = audioID
		r.paused, r.intentPaused = false, false
	case "pause", "play":
		if r.source == "" {
			return map[string]string{}, nil
		}
		r.revision++
		r.intentPaused = action == "pause"
	case "stop":
		r.revision++
		r.source = ""
		r.audioID = ""
		r.resolver = nil
		r.live, r.background, r.onLost = nil, nil, nil
		r.volume = nil
		r.paused, r.intentPaused = false, false
	}
	if action == "pause" || action == "play" || action == "stop" {
		a.renewLeasesLocked(r)
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
	var plans []speakerCommand
	for _, s := range targets {
		// Transport controls and metadata may only affect this route's speakers.
		if !r.owned[s.ID] || r.blocked[s.ID] {
			continue
		}
		if action != "play_url" && !r.started[s.ID] {
			continue
		}
		plans = append(plans, a.planLocked(route, r, s, tokens, action, volume))
		plans[len(plans)-1].primeVolume = plans[len(plans)-1].primeVolume || (action == "play_url" && r.live != nil)
	}
	revision := r.revision
	results := a.executeLocked(ctx, plans)
	if a.routes[route] == r && r.revision == revision && (action == "pause" || action == "play") {
		r.intentPaused = r.paused
	}
	if a.routes[route] == r && r.revision == revision && action == "stop" {
		for id := range r.owned {
			a.release(route, id)
		}
		r.owned = map[string]bool{}
		r.started = map[string]bool{}
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
	if r.source != source || !r.backgroundReady() {
		return nil
	}
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	var plans []speakerCommand
	for _, s := range a.config.Snapshot().RouteTargets(route) {
		if !ids[s.ID] || !r.owned[s.ID] || r.blocked[s.ID] {
			continue
		}
		plans = append(plans, a.planLocked(route, r, s, tokens, "play_url", 0))
		plans[len(plans)-1].reason = "retry"
	}
	return resultsError(a.executeLocked(ctx, plans))
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
	if !r.backgroundReady() || r.audioID == audioID {
		return nil
	}
	r.audioID = audioID
	return a.retryDisplayLocked(ctx, route, r)
}
func (a *App) reconcile(ctx context.Context, before, after store.Config) error {
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	var plans []speakerCommand
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
			if r.started[id] {
				plans = append(plans, a.planLocked(route, r, before.Speakers[id], tokens, "stop", 0))
			} else {
				a.release(route, id)
			}
			// Withdraw ownership even if offline: an obsolete route must never retry it.
			delete(r.owned, id)
			delete(r.started, id)
			// The stop retains its lease until execution; it cannot affect a
			// speaker acquired by another route while the network is pending.
		}
		if !r.backgroundReady() || tokens.UserID == "" {
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
			plans = append(plans, a.planLocked(route, r, s, tokens, "play_url", 0))
			plans[len(plans)-1].reason = "configuration"
			_, existed := old[id]
			plans[len(plans)-1].primeVolume = plans[len(plans)-1].primeVolume || (r.live != nil && !existed)
		}
		if len(r.owned) == 0 && r.onLost != nil {
			r.onLost()
			r.source, r.audioID, r.resolver = "", "", nil
			r.live, r.background, r.onLost = nil, nil, nil
			r.volume = nil
		}
	}
	results := a.executeLocked(ctx, plans)
	for _, p := range plans {
		if p.action == "stop" && a.leases[p.speaker.ID] == p.lease {
			a.release(p.route, p.speaker.ID)
		}
	}
	return resultsError(results)
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
		if lease := a.leases[id]; lease != nil {
			lease.cancel()
			delete(a.leases, id)
		}
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
