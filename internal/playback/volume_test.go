package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"micastgo/internal/app"
	"micastgo/internal/testsupport"
)

func TestVolumeCurveAndStereoRamp(t *testing.T) {
	if volumeGain(0) != 0 || volumeGain(100) != 1 || math.Abs(volumeGain(50)-math.Pow(10, -15.0/20)) > 1e-9 {
		t.Fatal("unexpected attenuation curve")
	}
	data := make([]byte, 442*4)
	for i := 0; i < len(data); i += 4 {
		binary.LittleEndian.PutUint16(data[i:], 30000)
		binary.LittleEndian.PutUint16(data[i+2:], uint16(65536-30000))
	}
	gain, previous := 1.0, int16(30000)
	applyGain(data, &gain, 0)
	for i := 0; i < len(data); i += 4 {
		left := int16(binary.LittleEndian.Uint16(data[i:]))
		right := int16(binary.LittleEndian.Uint16(data[i+2:]))
		if left != -right || left > previous || previous-left > 69 {
			t.Fatal("channel mismatch or abrupt gain", i, left, right)
		}
		previous = left
	}
	if gain != 0 || previous != 0 {
		t.Fatal("mute did not reach silence")
	}
}

func TestUnifiedVolumeOwnershipAndMute(t *testing.T) {
	e := lifecycleEngine(t)
	ctx := context.Background()
	if err := e.ProtocolVolume(ctx, 45, "dlna", ""); err != nil {
		t.Fatal(err)
	}
	old := e.newSession("dlna", Track{}, 0)
	if err := e.ProtocolMute(ctx, true, "dlna", old.identity); err != nil {
		t.Fatal(err)
	}
	if err := e.ProtocolVolume(ctx, 23, "dlna", old.identity); err != nil {
		t.Fatal(err)
	}
	if value, muted := e.VolumeState(); value != 23 || !muted || old.targetGain != 0 {
		t.Fatal("mute state not shared", value, muted)
	}
	if err := e.ProtocolMute(ctx, false, "dlna", old.identity); err != nil {
		t.Fatal(err)
	}
	if old.targetGain != volumeGain(23) {
		t.Fatal("mute lost restore volume")
	}
	if err := e.ProtocolMute(ctx, true, "dlna", old.identity); err != nil {
		t.Fatal(err)
	}
	current := e.newSession("airplay", Track{}, 0)
	for _, owner := range []string{old.identity, ""} {
		if err := e.ProtocolVolume(ctx, 90, "dlna", owner); err != nil {
			t.Fatal(err)
		}
		if err := e.ProtocolMute(ctx, true, "dlna", owner); err != nil {
			t.Fatal(err)
		}
	}
	if value, muted := e.VolumeState(); value != 23 || muted || current.targetGain != volumeGain(23) {
		t.Fatal("DLNA changed newer AirPlay cast")
	}
	if err := e.Volume(ctx, 61, current.identity); err != nil {
		t.Fatal(err)
	}
	if value, _ := e.VolumeState(); value != 61 {
		t.Fatal("AirPlay state not shared")
	}
}

func TestVolumeModeChangesOnlyForNewCast(t *testing.T) {
	e := lifecycleEngine(t)
	first := e.newSession("dlna", Track{}, 0)
	if first.volumeMode != "independent" {
		t.Fatal("default mode", first.volumeMode)
	}
	if err := e.app.UpdateSettings(context.Background(), "whole_house", "", "MiCast Go", "wav", app.SettingsOptions{VolumeMode: "linked"}); err != nil {
		t.Fatal(err)
	}
	resumed := e.newSession("dlna", Track{}, 2, first.identity)
	if resumed.volumeMode != "independent" {
		t.Fatal("resume changed mode mid cast")
	}
	if next := e.newSession("airplay", Track{}, 0); next.volumeMode != "linked" {
		t.Fatal("new cast did not adopt mode")
	}
}

func TestVolumeModesUseSamePipelineAndPrimeHardwareBeforePlay(t *testing.T) {
	for _, mode := range []string{"independent", "linked"} {
		t.Run(mode, func(t *testing.T) {
			a, cloud := testsupport.NewApp(t, "wav")
			if err := a.UpdateSettings(context.Background(), "whole_house", "", "MiCast Go", "wav", app.SettingsOptions{VolumeMode: mode}); err != nil {
				t.Fatal(err)
			}
			e := &Engine{ctx: context.Background(), app: a}
			t.Cleanup(e.Close)
			var mu sync.Mutex
			var volumes []int
			cloud.BeforeCommand = func(_ context.Context, _, method string, args map[string]any) error {
				if method == "player_set_volume" {
					mu.Lock()
					volumes = append(volumes, args["volume"].(int))
					mu.Unlock()
				}
				return nil
			}
			q := NewPCMQueue(e.ctx)
			owner, err := e.StartPCM(q, Track{}, 23)
			if err != nil {
				t.Fatal(err)
			}
			q.Push(make([]byte, 4096))
			select {
			case <-cloud.Plays:
			case <-time.After(3 * time.Second):
				t.Fatal("no play")
			}
			mu.Lock()
			initial := append([]int(nil), volumes...)
			mu.Unlock()
			want := 20
			if mode == "linked" {
				want = 23
			}
			if len(initial) != 1 || initial[0] != want {
				t.Fatal("incorrect volume before play", initial, want)
			}
			if err := e.Volume(context.Background(), 70, owner); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := append([]int(nil), volumes...)
			mu.Unlock()
			if mode == "independent" && len(got) != 1 {
				t.Fatal("software volume issued hardware command", got)
			}
			if mode == "linked" && (len(got) != 2 || got[1] != 70) {
				t.Fatal("linked volume not sent", got)
			}
			if a.Config().Speakers["speaker"].Volume != 20 {
				t.Fatal("phone overwrote saved speaker baseline")
			}
			if err := e.Pause(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			if err := e.Volume(context.Background(), 33, owner); err != nil {
				t.Fatal(err)
			}
			if err := e.Resume(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			resumed := append([]int(nil), volumes...)
			mu.Unlock()
			if mode == "linked" && (len(resumed) != 3 || resumed[2] != 33) {
				t.Fatal("paused volume not primed on resume", resumed)
			}
			if mode == "independent" && len(resumed) != 1 {
				t.Fatal("software resume changed hardware volume", resumed)
			}
		})
	}
}

func TestLinkedMuteFailureCannotStartOnRetry(t *testing.T) {
	a, cloud := testsupport.NewApp(t, "wav")
	if err := a.UpdateSettings(context.Background(), "whole_house", "", "MiCast Go", "wav", app.SettingsOptions{VolumeMode: "linked"}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{ctx: context.Background(), app: a}
	t.Cleanup(e.Close)
	cloud.BeforeCommand = func(_ context.Context, _, method string, args map[string]any) error {
		if method == "player_set_volume" && args["volume"] == 0 {
			return errors.New("volume unavailable")
		}
		return nil
	}
	q := NewPCMQueue(e.ctx)
	if _, err := e.StartPCM(q, Track{}, 0); err != nil {
		t.Fatal(err)
	}
	q.Push(make([]byte, 4096))
	deadline := time.Now().Add(2 * time.Second)
	for e.Status().State != "waiting_for_speakers" {
		if time.Now().After(deadline) {
			t.Fatal("prepare did not finish", e.Status())
		}
		time.Sleep(time.Millisecond)
	}
	e.op.Lock()
	source := "stream:" + e.current.id
	e.op.Unlock()
	if err := a.RetryRouteTargets(context.Background(), "", source, map[string]bool{"speaker": true}); err == nil {
		t.Fatal("retry ignored mute failure")
	}
	select {
	case <-cloud.Plays:
		t.Fatal("speaker played despite failed mute")
	default:
	}
}

type fragmentedPCM struct {
	data   *bytes.Reader
	cancel context.CancelFunc
}

func (r fragmentedPCM) Read(p []byte) (int, error) {
	n, err := r.data.Read(p[:min(3, len(p))])
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}

func TestPCMVolumePreservesFramesAcrossFragmentedReads(t *testing.T) {
	for _, mode := range []string{"independent", "linked"} {
		t.Run(mode, func(t *testing.T) {
			e := lifecycleEngine(t)
			s := e.newSession("dlna", Track{}, 0)
			s.volumeMode, s.targetGain = mode, .5
			s.hubs["pcm"] = newHub("pcm")
			data := []byte{0x10, 0x27, 0xf0, 0xd8, 0x20, 0x4e, 0xe0, 0xb1}
			e.consume(s, fragmentedPCM{bytes.NewReader(data), s.cancel})
			var got []byte
			for _, chunk := range s.hubs["pcm"].items {
				got = append(got, chunk.data...)
			}
			want := append([]byte(nil), data...)
			if mode == "independent" {
				gain := .5
				applyGain(want, &gain, .5)
			}
			if !bytes.Equal(got, want) || s.frames != 2 {
				t.Fatal("gain lost or corrupted PCM frames", got, want, s.frames)
			}
		})
	}
}
