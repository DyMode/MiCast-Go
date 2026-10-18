package playback

import (
	"testing"
	"time"
)

func TestRecoveryRequiresPersistentFaultAndVerifiedOutput(t *testing.T) {
	now := time.Now()
	r := &recoveryState{}
	p := Pull{Bytes: 100, LastRead: now.Add(-6 * time.Second)}
	if retry, _, _ := r.check(now, p, true); retry {
		t.Fatal("first fault restarted speaker")
	}
	if retry, _, _ := r.check(now.Add(3*time.Second), p, true); retry {
		t.Fatal("short interruption restarted speaker")
	}
	if retry, _, _ := r.check(now.Add(6*time.Second), p, true); !retry {
		t.Fatal("persistent fault not recovered")
	}
	if retry, recovered, health := r.check(now.Add(9*time.Second), p, true); retry || recovered || health != "recovering" {
		t.Fatal("command acceptance mistaken for recovery")
	}
	if retry, _, _ := r.check(now.Add(15*time.Second), p, true); retry {
		t.Fatal("cooldown ignored")
	}
	p.Bytes = 104
	p.LastRead = now.Add(16 * time.Second)
	if retry, recovered, _ := r.check(now.Add(17*time.Second), p, true); retry || !recovered || r.attempts != 0 {
		t.Fatal("renewed output not confirmed")
	}
	if retry, _, health := r.check(now.Add(30*time.Second), p, false); retry || health != "source_quiet" {
		t.Fatal("quiet source triggered speaker recovery")
	}
}

func TestDiagnosticEventsAreBounded(t *testing.T) {
	s := &session{}
	for i := 0; i < 100; i++ {
		s.record("recovery_requested", "speaker", i)
	}
	if len(s.events) != 48 || s.events[0].Attempt != 52 {
		t.Fatal("unbounded diagnostic history")
	}
}
