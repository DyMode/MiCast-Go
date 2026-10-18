package app

import (
	"context"
	"time"

	"micastgo/internal/xiaomi"
)

type Observation struct {
	xiaomi.PlayerStatus
	At     time.Time `json:"at"`
	Source string    `json:"-"`
	User   string    `json:"-"`
}

// Observation never changes playback intent or saved hardware volume.
func (a *App) ObserveRoute(ctx context.Context, route, source string) {
	reader, ok := a.cloud.(interface {
		PlayerStatus(context.Context, xiaomi.Tokens, string) (xiaomi.PlayerStatus, error)
	})
	if !ok {
		return
	}
	a.op.Lock()
	r := a.routes[route]
	if r == nil || r.source != source || source == "" {
		a.op.Unlock()
		return
	}
	type target struct {
		id    string
		lease *targetLease
		gate  chan struct{}
	}
	var targets []target
	for id := range r.owned {
		if r.started[id] {
			targets = append(targets, target{id, a.leases[id], a.commandGates[id]})
		}
	}
	a.mu.RLock()
	tokens, epoch := a.tokens, a.epoch
	a.mu.RUnlock()
	a.op.Unlock()
	for _, target := range targets {
		if target.lease == nil {
			continue
		}
		work, cancel := context.WithTimeout(ctx, 4*time.Second)
		stop := context.AfterFunc(target.lease.ctx, cancel)
		select {
		case target.gate <- struct{}{}:
		case <-work.Done():
			stop()
			cancel()
			continue
		}
		a.op.Lock()
		valid := a.routes[route] == r && r.source == source && a.leases[target.id] == target.lease && r.owned[target.id]
		a.op.Unlock()
		if !valid {
			<-target.gate
			stop()
			cancel()
			continue
		}
		status, err := reader.PlayerStatus(work, tokens, target.id)
		readActive := work.Err() == nil
		stop()
		cancel()
		a.op.Lock()
		valid = a.routes[route] == r && r.source == source && a.leases[target.id] == target.lease && r.owned[target.id]
		a.mu.Lock()
		if err == nil && readActive && valid && ctx.Err() == nil && target.lease.ctx.Err() == nil && a.epoch == epoch {
			if a.observations == nil {
				a.observations = map[string]Observation{}
			}
			a.observations[target.id] = Observation{status, time.Now(), source, tokens.UserID}
			if status.Volume != nil {
				a.rememberVolumeLocked(tokens.UserID, target.id, *status.Volume)
			}
		}
		a.mu.Unlock()
		a.op.Unlock()
		<-target.gate
	}
}
