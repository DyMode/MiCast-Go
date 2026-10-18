package playback

import (
	"context"
	"os"
	"sync"
)

type prefetch struct {
	mu               sync.Mutex
	owner, raw, path string
	cancel           context.CancelFunc
	done             chan struct{}
}

func (e *Engine) clearPrefetch(owner string) {
	e.prefetchMu.Lock()
	defer e.prefetchMu.Unlock()
	if p := e.prefetch; p != nil && (owner == "" || p.owner == owner) {
		p.cancel()
		e.prefetch = nil
	}
}

// One queued download; it never replaces the currently decoded cache.
func (e *Engine) PrefetchNext(owner, raw string) {
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.closing.Load() {
		return
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || s.mode != fileSource || owner == "" || s.identity != owner {
		return
	}
	e.prefetchMu.Lock()
	defer e.prefetchMu.Unlock()
	if e.prefetch != nil && e.prefetch.owner == owner && e.prefetch.raw == raw {
		return
	}
	if e.prefetch != nil {
		e.prefetch.cancel()
		e.prefetch = nil
	}
	if !validMediaURL(raw) || s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	p := &prefetch{owner: owner, raw: raw, cancel: cancel, done: make(chan struct{})}
	e.prefetch = p
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		path, err := e.fetchMedia(ctx, raw)
		p.mu.Lock()
		if err == nil {
			p.path = path
		}
		p.mu.Unlock()
		close(p.done)
		if err == nil {
			s.record("next_download_ready", "", 0)
		} else if ctx.Err() == nil {
			s.record("next_download_failed", "", 0)
		}
		<-ctx.Done()
		p.mu.Lock()
		path = p.path
		p.path = ""
		p.mu.Unlock()
		if path != "" {
			os.Remove(path)
		}
	}()
}

// Caller holds op. A new cast may consume only its predecessor's queued URI.
func (e *Engine) takePrefetch(raw string) *prefetch {
	e.prefetchMu.Lock()
	defer e.prefetchMu.Unlock()
	p := e.prefetch
	e.mu.RLock()
	current := e.current
	e.mu.RUnlock()
	if p != nil && p.raw == raw && current != nil && p.owner == current.identity && current.protocol == "dlna" {
		e.prefetch = nil
		return p
	}
	return nil
}
