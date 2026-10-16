// Package receivers publishes one independent pair of receivers per speaker,
// plus a separate whole-house pair.
package receivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"micastgo/internal/app"
	"micastgo/internal/dlna"
	"micastgo/internal/playback"
	"micastgo/internal/raop"
)

type Options struct {
	Host, Base, Binary, CacheDir, Identity, AirplayAddress string
	Interface                                              *net.Interface
	Discovery                                              bool
}
type Status struct {
	Targets        []string        `json:"targets"`
	Owned          []string        `json:"owned"`
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	SpeakerID      string          `json:"speaker_id,omitempty"`
	Airplay        string          `json:"airplay"`
	AirplayAddress string          `json:"airplay_address,omitempty"`
	DLNA           string          `json:"dlna"`
	DescriptionURL string          `json:"description_url"`
	Playback       playback.Status `json:"playback"`
}
type entry struct {
	id, name, owner, prefix string
	engine                  *playback.Engine
	airplay                 *raop.Server
	dlna                    *dlna.Server
	status                  Status
}

func (e *entry) close() {
	if e.airplay != nil {
		e.airplay.Close()
	}
	if e.dlna != nil {
		e.dlna.Close()
	}
	e.engine.Close()
}

type Manager struct {
	discovery      *dlna.Discovery
	discoveryError string
	ctx            context.Context
	cancel         context.CancelFunc
	app            *app.App
	options        Options
	syncMu         sync.Mutex
	mu             sync.RWMutex
	entries        map[string]*entry
	wg             sync.WaitGroup
}

func New(ctx context.Context, a *app.App, o Options) (*Manager, error) {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, app: a, options: o, entries: map[string]*entry{}}
	if _, err := exec.LookPath(o.Binary); err != nil {
		cancel()
		return nil, err
	}
	a.ReceiverState("audio", "ready")
	if o.Discovery {
		var err error
		m.discovery, err = dlna.NewDiscovery(ctx, o.Interface)
		if err != nil {
			m.discoveryError = err.Error()
		}
	}
	if err := m.Sync(); err != nil {
		m.Close()
		return nil, err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.Sync()
			}
		}
	}()
	return m, nil
}
func (m *Manager) Sync() error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.ctx.Err() != nil {
		return m.ctx.Err()
	}
	state := m.app.Snapshot()
	c := state.Config
	available := state.Account.UserID != "" && state.Account.Status != "expired"
	desired := map[string]string{}
	whole := available && len(c.RouteTargets("whole_house")) > 0
	if whole {
		name := c.Name

		desired["whole_house"] = name
	}
	for id, s := range c.Speakers {
		if available && s.Joined {
			desired["speaker:"+id] = s.Name
		}
	}
	m.mu.Lock()
	var removed []*entry
	for id, e := range m.entries {
		if name, keep := desired[id]; !keep || name != e.name || c.Owner != e.owner {
			delete(m.entries, id)
			removed = append(removed, e)
		}
	}
	m.mu.Unlock()
	for _, e := range removed {
		e.close()
	}
	var failures []error
	// The whole-house listener keeps its documented port and root URLs.
	ids := []string{}
	if whole {
		ids = append(ids, "whole_house")
	}
	for id := range desired {
		if id != "whole_house" {
			ids = append(ids, id)
		}
	}
	if whole {
		sort.Strings(ids[1:])
	} else {
		sort.Strings(ids)
	}
	for _, id := range ids {
		m.mu.RLock()
		exists := m.entries[id] != nil
		m.mu.RUnlock()
		if exists {
			continue
		}
		e, err := m.open(id, desired[id], c.Owner)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		m.mu.Lock()
		m.entries[id] = e
		m.mu.Unlock()
	}
	return errors.Join(failures...)
}
func (m *Manager) open(id, name, owner string) (*entry, error) {
	o := m.options
	digest := sha256.Sum256([]byte(owner + "/" + id))
	slug := hex.EncodeToString(digest[:12])
	prefix := "/receivers/" + slug
	kind, speaker := "speaker", strings.TrimPrefix(id, "speaker:")
	address := o.AirplayAddress
	if id == "whole_house" {
		prefix = ""
		kind = "whole_house"
		speaker = ""
	} else {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		address = net.JoinHostPort(host, "0")
	}
	base := strings.TrimRight(o.Base, "/") + prefix
	engine, err := playback.New(m.ctx, m.app, o.Binary, base, filepath.Join(o.CacheDir, slug), id)
	if err != nil {
		return nil, err
	}
	e := &entry{id: id, name: name, owner: owner, prefix: prefix, engine: engine, status: Status{ID: id, Name: name, Kind: kind, SpeakerID: speaker, Airplay: "ready", DLNA: "ready", DescriptionURL: base + "/dlna/description.xml"}}
	e.airplay, err = raop.Start(m.ctx, engine, name, o.Host, address, o.Discovery, o.Identity+"/"+owner+"/"+id)
	if err != nil {
		e.status.Airplay = "error: " + err.Error()
	} else {
		e.status.AirplayAddress = e.airplay.Addr().String()
	}
	e.dlna = dlna.New(m.ctx, engine, name, base, o.Identity+"/"+owner+"/"+id)
	if m.discovery != nil {
		m.discovery.Add(e.dlna)
	} else if m.discoveryError != "" {
		e.status.DLNA = "discovery_error: " + m.discoveryError
	}
	if id == "whole_house" {
		m.app.ReceiverState("audio", "ready")
		m.app.ReceiverState("airplay", e.status.Airplay)
		m.app.ReceiverState("dlna", e.status.DLNA)
	}
	return e, nil
}
func (m *Manager) Engine(id string) (*playback.Engine, error) {
	if id == "" {
		id = "whole_house"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.entries[id]
	if e == nil {
		return nil, errors.New("投送入口未开启，请先启用音箱")
	}
	return e.engine, nil
}
func (m *Manager) Status() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []Status{}
	for _, e := range m.entries {
		s := e.status
		s.Playback = e.engine.Status()
		s.Targets = []string{}
		for _, sp := range m.app.Config().RouteTargets(e.id) {
			s.Targets = append(s.Targets, sp.ID)
		}
		sort.Strings(s.Targets)
		s.Owned = m.app.RouteMembers(e.id)
		sort.Strings(s.Owned)
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind == "whole_house"
		}
		return result[i].Name < result[j].Name
	})
	return result
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	m.mu.RLock()
	var selected *entry
	if strings.HasPrefix(path, "/dlna/") || strings.HasPrefix(path, "/stream/") {
		selected = m.entries["whole_house"]
	} else {
		for _, e := range m.entries {
			if e.prefix != "" && strings.HasPrefix(path, e.prefix+"/") {
				selected = e
				break
			}
		}
	}
	m.mu.RUnlock()
	if selected == nil {
		http.NotFound(w, r)
		return
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	clone.URL.Path = strings.TrimPrefix(path, selected.prefix)
	if strings.HasPrefix(clone.URL.Path, "/dlna/") {
		selected.dlna.ServeHTTP(w, clone)
	} else if strings.HasPrefix(clone.URL.Path, "/stream/") {
		selected.engine.ServeHTTP(w, clone)
	} else {
		http.NotFound(w, r)
	}
}
func (m *Manager) StopAll() {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.RUnlock()
	for _, e := range entries {
		e.engine.Stop("")
	}
}
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	entries := m.entries
	m.entries = map[string]*entry{}
	m.mu.Unlock()
	var closing sync.WaitGroup
	for _, e := range entries {
		closing.Add(1)
		go func(e *entry) { defer closing.Done(); e.close() }(e)
	}
	closing.Wait()
	if m.discovery != nil {
		m.discovery.Close()
	}
}
