package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"micastgo/internal/store"
	"micastgo/internal/version"
	"micastgo/internal/xiaomi"
)

type Cloud interface {
	StartQR(context.Context) (xiaomi.QR, error)
	PollQR(context.Context, xiaomi.QR) (string, *xiaomi.Tokens, xiaomi.QR, error)
	Exchange(context.Context, xiaomi.Tokens) (xiaomi.Tokens, error)
	Devices(context.Context, xiaomi.Tokens) ([]xiaomi.Device, error)
	Command(context.Context, xiaomi.Tokens, string, string, map[string]any) error
	PlayURL(context.Context, xiaomi.Tokens, string, string, string, string) error
}
type QRSession struct {
	ID        string    `json:"id"`
	LoginURL  string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	qr        xiaomi.QR
	epoch     uint64
}
type DeviceState struct {
	CurrentVolume *int                      `json:"current_volume,omitempty"`
	Formats       map[string]FormatEvidence `json:"formats,omitempty"`
	Observed      *Observation              `json:"observed,omitempty"`
	store.Speaker
	Discovered   bool   `json:"discovered"`
	CommandState string `json:"command_state,omitempty"`
	Error        string `json:"error,omitempty"`
}
type AccountState struct {
	Status      string    `json:"status"`
	UserID      string    `json:"user_id,omitempty"`
	Error       string    `json:"error,omitempty"`
	RefreshedAt time.Time `json:"refreshed_at,omitempty"`
}
type Snapshot struct {
	Version   string            `json:"version"`
	Account   AccountState      `json:"account"`
	Config    store.Config      `json:"config"`
	Devices   []DeviceState     `json:"devices"`
	Receivers map[string]string `json:"receivers"`
}
type App struct {
	volumes         map[string]int                       // Protected by mu; latest command/readback, not persisted defaults.
	formats         map[string]map[string]FormatEvidence // Protected by mu; bounded session evidence.
	observations    map[string]Observation               // Protected by mu.
	nextRefresh     time.Time
	refreshFailures int
	lastRecovery    time.Time
	ownerMu         sync.RWMutex // Ownership reads never wait for cloud requests.

	mu            sync.RWMutex
	op            sync.Mutex // Orders configuration changes and cloud playback commands.
	authOp        sync.Mutex // Only one account exchange or QR poll at a time.
	refreshOp     sync.Mutex // Device fetches do not hold the playback state lock.
	cloud         Cloud
	config        *store.ConfigStore
	vault         *store.Vault
	tokens        xiaomi.Tokens
	epoch         uint64
	qr            *QRSession
	status        string
	lastError     string
	discovered    map[string]bool
	states        map[string]string
	commandErrors map[string]string
	lastRefresh   time.Time
	nextRenew     time.Time
	routes        map[string]*routeState // Protected by op.
	commandGates  map[string]chan struct{}
	leases        map[string]*targetLease
	owners        map[string]string // Physical speaker -> route; protected by op.
	receivers     map[string]string
}

func New(config *store.ConfigStore, vault *store.Vault, cloud Cloud) (*App, error) {
	a := &App{config: config, vault: vault, cloud: cloud, status: "disconnected", discovered: map[string]bool{}, states: map[string]string{}, commandErrors: map[string]string{}, routes: map[string]*routeState{}, owners: map[string]string{}}
	err := vault.Load(&a.tokens)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if a.tokens.UserID != "" && a.tokens.ServiceToken != "" {
		a.status = "stored"
		if a.tokens.Rejected {
			a.status = "expired"
		}
	}
	return a, nil
}
func (a *App) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	c := a.config.Snapshot()
	devices := []DeviceState{}
	for id, s := range c.Speakers {
		device := DeviceState{Speaker: s, Discovered: a.discovered[id], CommandState: a.states[id], Error: a.commandErrors[id]}
		if observation, ok := a.observations[id]; ok && observation.User == a.tokens.UserID {
			device.Observed = &observation
		}
		if evidence := a.formats[a.tokens.UserID+"/"+id]; len(evidence) > 0 {
			device.Formats = make(map[string]FormatEvidence, len(evidence))
			for format, value := range evidence {
				device.Formats[format] = value
			}
		}
		if value, ok := a.volumes[a.tokens.UserID+"/"+id]; ok {
			v := value
			device.CurrentVolume = &v
		}
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	// Stored credentials do not prove cloud connectivity or speaker reachability.
	states := map[string]string{"airplay": "stopped", "dlna": "stopped", "audio": "stopped"}
	for k, v := range a.receivers {
		states[k] = v
	}
	return Snapshot{version.Version, AccountState{a.status, a.tokens.UserID, a.lastError, a.tokens.RefreshedAt}, c, devices, states}
}
func (a *App) Config() store.Config { return a.config.Snapshot() }
func (a *App) ReceiverState(name, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.receivers == nil {
		a.receivers = map[string]string{}
	}
	a.receivers[name] = state
}
func (a *App) StartQR(ctx context.Context) (QRSession, error) {
	a.authOp.Lock()
	defer a.authOp.Unlock()
	a.mu.Lock()
	a.epoch++
	epoch := a.epoch
	a.qr = nil
	a.mu.Unlock()
	qr, err := a.cloud.StartQR(ctx)
	if err != nil {
		return QRSession{}, err
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return QRSession{}, err
	}
	session := QRSession{hex.EncodeToString(b), qr.LoginURL, time.Now().Add(3 * time.Minute), qr, epoch}
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.epoch {
		return QRSession{}, errors.New("登录请求已取消")
	}
	a.qr = &session
	return session, nil
}
func (a *App) QRLoginURL(id string) (string, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.qr == nil || a.qr.ID != id || time.Now().After(a.qr.ExpiresAt) {
		return "", errors.New("二维码已过期，请重新获取")
	}
	return a.qr.LoginURL, nil
}
func (a *App) PollQR(ctx context.Context, id string) (string, error) {
	if !a.authOp.TryLock() {
		return "waiting", nil
	}
	defer a.authOp.Unlock()
	a.mu.RLock()
	if a.qr == nil || a.qr.ID != id || time.Now().After(a.qr.ExpiresAt) {
		a.mu.RUnlock()
		return "expired", nil
	}
	qr := *a.qr
	a.mu.RUnlock()
	status, tokens, next, err := a.cloud.PollQR(ctx, qr.qr)
	if err != nil {
		return "", err
	}
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.epoch != qr.epoch || a.qr == nil || a.qr.ID != id {
		return "expired", nil
	}
	a.qr.qr = next
	if status == "expired" {
		a.qr = nil
		return status, nil
	}
	if status == "confirmed" {
		if tokens == nil || tokens.UserID == "" || tokens.ServiceToken == "" {
			return "", errors.New("无效的登录凭证")
		}
		// Account changes must not inherit a different household's devices.
		if err = a.config.Update(func(c *store.Config) error {
			if c.Owner != tokens.UserID {
				c.Speakers = map[string]store.Speaker{}
				c.Selected = ""
			}
			c.Owner = tokens.UserID
			return nil
		}); err != nil {
			return "", err
		}
		if err = a.vault.Save(tokens); err != nil {
			return "", err
		}
		a.tokens = *tokens
		a.status = "stored"
		a.lastError = ""
		a.qr = nil
		a.discovered = map[string]bool{}
		a.states = map[string]string{}
		a.commandErrors = map[string]string{}
		a.resetRoutesLocked()
		a.nextRenew = time.Time{}
	}
	return status, nil
}
func (a *App) Logout() error {
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.vault.Clear(); err != nil {
		return err
	}
	a.epoch++
	a.qr = nil
	a.tokens = xiaomi.Tokens{}
	a.status = "disconnected"
	a.lastError = ""
	a.discovered = map[string]bool{}
	a.states = map[string]string{}
	a.commandErrors = map[string]string{}
	a.resetRoutesLocked()
	a.nextRenew = time.Time{}
	return nil
}
func (a *App) Refresh(ctx context.Context, force bool) error {
	if !a.refreshOp.TryLock() {
		return nil
	}
	defer a.refreshOp.Unlock()
	a.op.Lock()
	a.mu.RLock()
	tokens, epoch, last, status := a.tokens, a.epoch, a.lastRefresh, a.status
	a.mu.RUnlock()
	if tokens.UserID == "" || status == "expired" {
		a.op.Unlock()
		return nil
	}
	if !force && (time.Since(last) < 5*time.Minute || time.Now().Before(a.nextRefresh)) {
		a.op.Unlock()
		return nil
	}
	a.op.Unlock()
	devices, err := a.cloud.Devices(ctx, tokens)
	a.op.Lock()
	a.mu.RLock()
	sameAccount := a.epoch == epoch && a.tokens.UserID == tokens.UserID
	a.mu.RUnlock()
	recoverAccount := sameAccount && err != nil && !errors.Is(err, xiaomi.ErrRateLimited) && tokens.PassToken != "" && time.Since(a.lastRecovery) >= time.Minute && a.authOp.TryLock()
	if recoverAccount {
		defer a.authOp.Unlock()
		a.lastRecovery = time.Now()
	}
	a.op.Unlock()
	var recovered *xiaomi.Tokens
	var rejected error
	if recoverAccount {
		next, recoveryErr := a.cloud.Exchange(ctx, tokens)
		if recoveryErr == nil && next.UserID == tokens.UserID && next.ServiceToken != "" {
			recovered = &next
			devices, err = a.cloud.Devices(ctx, next)
		} else if errors.Is(recoveryErr, xiaomi.ErrRejected) {
			rejected = recoveryErr
		}
	}
	a.op.Lock()
	defer a.op.Unlock()
	before := a.config.Snapshot()
	reconcile := false
	a.mu.Lock()
	defer func() {
		a.mu.Unlock()
		if reconcile {
			_ = a.reconcile(ctx, before, a.config.Snapshot())
		}
	}()
	if epoch != a.epoch {
		return nil
	}
	if rejected != nil {
		a.tokens.Rejected = true
		a.status, a.lastError = "expired", rejected.Error()
		_ = a.vault.Save(a.tokens)
		return rejected
	}
	if recovered != nil {
		if saveErr := a.vault.Save(*recovered); saveErr != nil {
			return saveErr
		}
		a.tokens = *recovered
	}
	a.lastRefresh = time.Now()
	if err != nil {
		a.refreshFailures++
		a.nextRefresh = time.Now().Add(min(time.Hour, 5*time.Minute*time.Duration(1<<min(a.refreshFailures-1, 4))))
		if errors.Is(err, xiaomi.ErrRateLimited) {
			a.nextRefresh = time.Now().Add(30 * time.Minute)
		}
		a.status = "unstable"
		a.lastError = err.Error()
		a.discovered = map[string]bool{}
		return err
	}
	if err = a.config.Update(func(c *store.Config) error {
		if c.Owner != tokens.UserID {
			c.Speakers = map[string]store.Speaker{}
			c.Selected = ""
			c.Owner = tokens.UserID
		}
		for _, d := range devices {
			if d.ID == "" || d.Hardware == "" {
				continue
			}
			s, exists := c.Speakers[d.ID]
			if !exists {
				s = store.Speaker{ID: d.ID, Joined: true, WholeHouse: true, Volume: 30, Format: "auto"}
			}
			s.Name = d.Name
			if s.Name == "" {
				s.Name = d.Hardware
			}
			s.Hardware = d.Hardware
			c.Speakers[d.ID] = s
		}
		return nil
	}); err != nil {
		return err
	}
	a.discovered = map[string]bool{}
	for _, d := range devices {
		a.discovered[d.ID] = true
	}
	a.refreshFailures = 0
	a.nextRefresh = time.Time{}
	a.status = "connected"
	a.lastError = ""
	reconcile = true
	return nil
}

func (a *App) Renew(ctx context.Context) error {
	if !a.authOp.TryLock() {
		return nil
	}
	defer a.authOp.Unlock()
	a.mu.RLock()
	tokens, epoch, nextAttempt, status := a.tokens, a.epoch, a.nextRenew, a.status
	a.mu.RUnlock()
	if tokens.PassToken == "" || status == "expired" || time.Now().Before(nextAttempt) || time.Since(tokens.RefreshedAt) < 7*24*time.Hour {
		return nil
	}
	a.mu.Lock()
	a.nextRenew = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	next, err := a.cloud.Exchange(ctx, tokens)
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.epoch {
		return nil
	}
	if err != nil {
		a.nextRenew = time.Now().Add(12 * time.Hour)
		a.lastError = err.Error()
		if errors.Is(err, xiaomi.ErrRejected) {
			a.status = "expired"
			a.tokens.Rejected = true
			_ = a.vault.Save(a.tokens)
		} else {
			a.status = "unstable"
		}
		return err
	}
	if next.UserID != tokens.UserID || next.ServiceToken == "" {
		return errors.New("续期回应无效，已保留原凭证")
	}
	if err = a.vault.Save(next); err != nil {
		return err
	}
	a.tokens = next
	a.nextRenew = time.Time{}
	a.status = "stored"
	a.lastError = ""
	return nil
}

type SpeakerPatch struct {
	Joined     *bool   `json:"joined"`
	WholeHouse *bool   `json:"whole_house"`
	Volume     *int    `json:"volume"`
	DelayMS    *int    `json:"delay_ms"`
	Format     *string `json:"format"`
	Muted      *bool   `json:"muted"`
}

func (a *App) UpdateSpeaker(ctx context.Context, id string, p SpeakerPatch) error {
	a.op.Lock()
	defer a.op.Unlock()
	before := a.config.Snapshot()
	_, ok := before.Speakers[id]
	if !ok {
		return errors.New("音箱不存在")
	}
	err := a.config.Update(func(c *store.Config) error {
		s := c.Speakers[id]
		if p.Joined != nil {
			s.Joined = *p.Joined
		}
		if p.WholeHouse != nil {
			s.WholeHouse = *p.WholeHouse
		}
		if p.Volume != nil {
			s.Volume = *p.Volume
		}
		if p.DelayMS != nil {
			s.DelayMS = *p.DelayMS
		}
		if p.Format != nil {
			s.Format = *p.Format
		}
		if p.Muted != nil {
			s.Muted = *p.Muted
		}
		c.Speakers[id] = s
		return nil
	})
	if err != nil {
		return err
	}
	next := a.config.Snapshot().Speakers[id]
	err = a.reconcile(ctx, before, a.config.Snapshot())
	currentConfig := a.config.Snapshot()
	if currentConfig.Owner != before.Owner {
		return err
	}
	next = currentConfig.Speakers[id]
	if p.Volume != nil || p.Muted != nil {
		route, owned := a.owner(id)
		r := a.routes[route]
		if !owned || r == nil || !r.runnable() {
			a.mu.Lock()
			v := next.Volume
			if next.Muted {
				v = 0
			}
			a.rememberVolumeLocked(a.tokens.UserID, id, v)
			a.mu.Unlock()
		}
	}
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	if (p.Volume != nil || p.Muted != nil) && next.Joined && tokens.UserID != "" {
		volume := next.Volume
		if next.Muted {
			volume = 0
		}
		if route, owned := a.owner(id); owned {
			r := a.routes[route]
			if r != nil && r.runnable() {
				plan := a.planLocked(route, r, next, tokens, "volume", volume)
				err = errors.Join(err, resultsError(a.executeLocked(ctx, []speakerCommand{plan})))
			}
		}
	}
	return err
}

type SettingsOptions struct {
	VolumeMode string
	Equalizer  *store.Equalizer
}

func (a *App) UpdateSettings(ctx context.Context, mode, selected, name, format string, options ...SettingsOptions) error {
	a.op.Lock()
	defer a.op.Unlock()
	before := a.config.Snapshot()
	err := a.config.Update(func(c *store.Config) error {
		c.Mode = mode
		c.Selected = selected
		c.Name = name
		c.Format = format
		if len(options) > 0 {
			if options[0].VolumeMode != "" {
				c.VolumeMode = options[0].VolumeMode
			}
			if options[0].Equalizer != nil {
				c.Equalizer = *options[0].Equalizer
			}
		}
		if mode == "single" {
			s, ok := c.Speakers[selected]
			if !ok || !s.Joined {
				return errors.New("请选择已加入的音箱")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.reconcile(ctx, before, a.config.Snapshot())
}

func (a *App) recordCommand(id, state string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.commandErrors[id] = err.Error()
		a.states[id] = "failed"
	} else {
		delete(a.commandErrors, id)
		if state != "" {
			a.states[id] = state
		}
	}
}

func (a *App) SearchTrack(ctx context.Context, title, artist string) (string, error) {
	a.mu.RLock()
	tokens := a.tokens
	a.mu.RUnlock()
	searcher, ok := a.cloud.(interface {
		SearchTrack(context.Context, xiaomi.Tokens, string, string) (string, error)
	})
	if !ok {
		return "", nil
	}
	return searcher.SearchTrack(ctx, tokens, title, artist)
}
func (a *App) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 25*time.Second)
		_ = a.Renew(work)
		cancel()
		work, cancel = context.WithTimeout(ctx, 15*time.Second)
		_ = a.Refresh(work, false)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) ClearEntries() error {
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status != "expired" && a.tokens.UserID != "" {
		return errors.New("请先退出米家或等待登录失效")
	}
	if err := a.config.Update(func(c *store.Config) error {
		c.Speakers = map[string]store.Speaker{}
		c.Selected = ""
		c.Mode = "whole_house"
		return nil
	}); err != nil {
		return err
	}
	a.discovered = map[string]bool{}
	a.states = map[string]string{}
	a.commandErrors = map[string]string{}
	return nil
}
