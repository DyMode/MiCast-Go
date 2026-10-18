package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
)

var ErrSuperseded = errors.New("播放操作已取消或被新的投放替代")

type targetLease struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type speakerCommand struct {
	route, action, url, audioID string
	r                           *routeState
	revision                    uint64
	speaker                     store.Speaker
	tokens                      xiaomi.Tokens
	lease                       *targetLease
	gate                        chan struct{}
	volume                      int
	epoch                       uint64
	reason                      string
	primeVolume                 bool
}

func (r *routeState) runnable() bool {
	return r.source != "" && !r.paused && !r.intentPaused && (r.live == nil || r.live())
}

func (r *routeState) backgroundReady() bool {
	return r.runnable() && (r.background == nil || r.background())
}

// State planning is short and global. Network execution is serialized only
// per physical speaker; takeover cancels its old lease without waiting on IO.
func (a *App) planLocked(route string, r *routeState, s store.Speaker, tokens xiaomi.Tokens, action string, volume int) speakerCommand {
	if a.commandGates == nil {
		a.commandGates = map[string]chan struct{}{}
	}
	gate := a.commandGates[s.ID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		a.commandGates[s.ID] = gate
	}
	p := speakerCommand{route: route, r: r, revision: r.revision, speaker: s, tokens: tokens, action: action, volume: volume, lease: a.leases[s.ID], gate: gate, audioID: r.audioID}
	a.mu.RLock()
	p.epoch = a.epoch
	a.mu.RUnlock()
	p.reason = "control"
	if action == "play_url" {
		p.url = r.url(s)
	}
	if action == "play_url" || action == "play" {
		if r.volume != nil {
			_, p.primeVolume = r.volume()
		}
	}
	return p
}

func (a *App) validCommandLocked(p speakerCommand) bool {
	a.mu.RLock()
	authorized := a.epoch == p.epoch && a.tokens.UserID == p.tokens.UserID && a.status != "expired"
	a.mu.RUnlock()
	if !authorized {
		return false
	}
	if a.routes[p.route] != p.r || p.r.revision != p.revision || a.leases[p.speaker.ID] != p.lease || p.lease == nil || !a.RouteOwns(p.route, p.speaker.ID) {
		return false
	}
	if p.action == "stop" {
		return true
	}
	if !p.r.owned[p.speaker.ID] || p.r.blocked[p.speaker.ID] {
		return false
	}
	if p.action == "play_url" || p.action == "volume" {
		if p.reason == "retry" || p.reason == "metadata" || p.reason == "configuration" {
			return p.r.backgroundReady()
		}
		return p.r.runnable()
	}
	return p.r.source != ""
}

// Caller holds op on entry and return. Every command rechecks its generation
// after acquiring the speaker gate; stale completions cannot commit state.
func (a *App) executeLocked(ctx context.Context, plans []speakerCommand) map[string]string {
	a.op.Unlock()
	results := map[string]string{}
	var mu sync.Mutex
	var workers sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, p := range plans {
		workers.Add(1)
		go func(p speakerCommand) {
			defer workers.Done()
			if p.lease == nil {
				return
			}
			work, cancel := context.WithCancel(ctx)
			stopCancel := context.AfterFunc(p.lease.ctx, cancel)
			defer func() { stopCancel(); cancel() }()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-work.Done():
				return
			}
			select {
			case p.gate <- struct{}{}:
				defer func() { <-p.gate }()
			case <-work.Done():
				return
			}
			a.op.Lock()
			valid := work.Err() == nil && a.validCommandLocked(p)
			if valid && p.action == "play_url" {
				p.r.started[p.speaker.ID] = true
			}
			a.op.Unlock()
			if !valid {
				return
			}
			slog.Debug("音箱命令", "route", p.route, "speaker", p.speaker.ID, "action", p.action)
			var err error
			var appliedVolume *int
			if p.primeVolume {
				a.op.Lock()
				active := work.Err() == nil && a.validCommandLocked(p)
				v := p.speaker.Volume
				if active && p.r.volume != nil {
					if value, ok := p.r.volume(); ok {
						v = value
					}
				}
				if active && (p.action == "play" || p.reason == "retry" || p.reason == "metadata" || p.r.continuation) {
					a.mu.RLock()
					known, ok := a.volumes[p.tokens.UserID+"/"+p.speaker.ID]
					a.mu.RUnlock()
					if ok {
						v = known
					}
				}
				a.op.Unlock()
				if active {
					if p.speaker.Muted {
						v = 0
					}
					if volumeErr := a.cloud.Command(work, p.tokens, p.speaker.ID, "player_set_volume", map[string]any{"media": "app_ios", "volume": v}); volumeErr != nil {
						slog.Warn("音箱初始音量设置失败", "speaker", p.speaker.ID, "reason", volumeErr)
						if v == 0 {
							err = volumeErr
						}
					} else {
						level := v
						appliedVolume = &level
					}
				}
			}
			state := "command_accepted"
			switch p.action {
			case "play_url":
				a.op.Lock()
				active := work.Err() == nil && a.validCommandLocked(p)
				a.op.Unlock()
				if active && err == nil {
					slog.Info("音箱播放指令", "route", p.route, "speaker", p.speaker.ID, "reason", p.reason, "generation", p.revision)
					err = a.cloud.PlayURL(work, p.tokens, p.speaker.ID, p.speaker.Hardware, p.url, p.audioID)
				}
			case "volume":
				v := p.volume
				if p.speaker.Muted {
					v = 0
				}
				err = a.cloud.Command(work, p.tokens, p.speaker.ID, "player_set_volume", map[string]any{"media": "app_ios", "volume": v})
				if err == nil {
					level := v
					appliedVolume = &level
				}
				state = ""
			default:
				if err == nil {
					err = a.cloud.Command(work, p.tokens, p.speaker.ID, "player_play_operation", map[string]any{"media": "app_ios", "action": p.action})
				}
				if p.action == "pause" {
					state = "paused"
				}
				if p.action == "stop" {
					state = "stopped"
				}
			}
			a.op.Lock()
			valid = a.validCommandLocked(p)
			if valid {
				a.recordCommand(p.speaker.ID, state, err)
				if appliedVolume != nil {
					a.mu.Lock()
					a.rememberVolumeLocked(p.tokens.UserID, p.speaker.ID, *appliedVolume)
					a.mu.Unlock()
				}
				if err == nil && (p.action == "volume" || p.primeVolume) {
					a.mu.Lock()
					delete(a.observations, p.speaker.ID)
					a.mu.Unlock()
				}
				if err == nil && (p.action == "pause" || p.action == "play") {
					p.r.paused = p.action == "pause"
				}
			}
			a.op.Unlock()
			if !valid {
				return
			}
			mu.Lock()
			if err != nil {
				results[p.speaker.ID] = err.Error()
			} else {
				results[p.speaker.ID] = "accepted"
			}
			mu.Unlock()
		}(p)
	}
	workers.Wait()
	a.op.Lock()
	return results
}

func resultsError(results map[string]string) error {
	var failures []error
	for _, v := range results {
		if v != "accepted" {
			failures = append(failures, errors.New(v))
		}
	}
	return errors.Join(failures...)
}

func (a *App) retryDisplayLocked(ctx context.Context, route string, r *routeState) error {
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	var plans []speakerCommand
	for _, s := range a.config.Snapshot().RouteTargets(route) {
		if r.owned[s.ID] && !r.blocked[s.ID] {
			plans = append(plans, a.planLocked(route, r, s, tokens, "play_url", 0))
			plans[len(plans)-1].reason = "metadata"
		}
	}
	return resultsError(a.executeLocked(ctx, plans))
}

func (a *App) resetRoutesLocked() {
	for _, lease := range a.leases {
		lease.cancel()
	}
	for _, r := range a.routes {
		if r.onLost != nil {
			r.onLost()
		}
	}
	a.leases = map[string]*targetLease{}
	a.routes = map[string]*routeState{}
	a.ownerMu.Lock()
	a.owners = map[string]string{}
	a.ownerMu.Unlock()
}

// Pause intent cancels queued/in-flight starts without killing the decoder.
func (a *App) SetRouteIntent(route, source string, paused bool) {
	a.op.Lock()
	defer a.op.Unlock()
	r := a.routes[route]
	if r == nil || r.source != source {
		return
	}
	r.intentPaused = paused
	if paused {
		r.revision++
		a.renewLeasesLocked(r)
	}
}

func (a *App) renewLeasesLocked(r *routeState) {
	for id := range r.owned {
		if lease := a.leases[id]; lease != nil {
			lease.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.leases[id] = &targetLease{ctx: ctx, cancel: cancel}
	}
}
