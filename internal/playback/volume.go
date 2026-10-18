package playback

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
)

// Protocols normalize their input to 0..100. State and execution belong here.
func volumeGain(value int) float64 {
	if value <= 0 {
		return 0
	}
	return math.Pow(10, (float64(min(value, 100))-100)*30/100/20)
}

// Smooth both channels together over about 10ms; never amplify the source.
func applyGain(data []byte, current *float64, target float64) {
	const step = 1.0 / 441
	for i := 0; i+3 < len(data); i += 4 {
		*current += max(-step, min(step, target-*current))
		for c := 0; c < 4; c += 2 {
			v := int16(binary.LittleEndian.Uint16(data[i+c:]))
			binary.LittleEndian.PutUint16(data[i+c:], uint16(int16(math.Round(float64(v)**current))))
		}
	}
}

func (e *Engine) effectiveVolume() int {
	if e.requestedMuted {
		return 0
	}
	if e.requestedVolume == nil {
		return 100
	}
	return *e.requestedVolume
}

func (e *Engine) VolumeState() (int, bool) {
	e.volumeMu.Lock()
	defer e.volumeMu.Unlock()
	value := 100
	if e.requestedVolume != nil {
		value = *e.requestedVolume
	}
	return value, e.requestedMuted
}

func (e *Engine) Volume(ctx context.Context, value int, owner ...string) error {
	return e.setVolume(ctx, &value, nil, "", owner)
}

func (e *Engine) ProtocolVolume(ctx context.Context, value int, protocol, owner string) error {
	return e.setVolume(ctx, &value, nil, protocol, []string{owner})
}

func (e *Engine) ProtocolMute(ctx context.Context, mute bool, protocol, owner string) error {
	return e.setVolume(ctx, nil, &mute, protocol, []string{owner})
}

func (e *Engine) setVolume(ctx context.Context, value *int, mute *bool, protocol string, owner []string) error {
	e.op.Lock()
	defer e.op.Unlock()
	if value != nil && (*value < 0 || *value > 100) {
		return errors.New("invalid volume")
	}
	if e.closed || e.closing.Load() {
		return errors.New("receiver closed")
	}
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if protocol != "" {
		// Idle commands may remember a setting, but cannot control another protocol.
		if s != nil && s.ctx.Err() == nil && (s.protocol != protocol || len(owner) == 0 || !ownsSession(s, owner)) {
			return nil
		}
	} else if len(owner) > 0 && (s == nil || !ownsSession(s, owner) || s.ctx.Err() != nil) {
		return nil
	}
	if s != nil && s.ctx.Err() == nil && s.volumeMode == "linked" {
		e.volumeMu.Lock()
		next := e.effectiveVolume()
		muted := e.requestedMuted
		remembered := e.requestedVolume
		e.volumeMu.Unlock()
		if mute != nil {
			muted = *mute
		}
		if value != nil {
			next = *value
		}
		if muted {
			next = 0
		} else if mute != nil && !*mute && value == nil {
			next = 100
			if remembered != nil {
				next = *remembered
			}
		}
		results, err := e.app.ControlRouteSource(ctx, e.route, "stream:"+s.id, "volume", next)
		if err = commandError(results, err); err != nil {
			return err
		}
	}
	e.volumeMu.Lock()
	if value != nil {
		v := *value
		e.requestedVolume = &v
	}
	if mute != nil {
		e.requestedMuted = *mute
	}
	e.volumeMu.Unlock()
	if s != nil {
		s.mu.Lock()
		e.volumeMu.Lock()
		v := e.effectiveVolume()
		e.volumeMu.Unlock()
		s.hardwareVolume = &v
		s.targetGain = volumeGain(v)
		s.mu.Unlock()
	}
	return nil
}
