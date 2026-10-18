package app

import (
	"micastgo/internal/xiaomi"
	"testing"
)

func TestFormatEvidenceIsAccountScopedAndNeverClaimsSound(t *testing.T) {
	a := newApp(t, &fakeCloud{devices: []xiaomi.Device{{ID: "one"}}})
	a.RecordFormat("wrong", "one", "pcm")
	if len(a.formats) != 0 {
		t.Fatal("wrong account recorded capability")
	}
	a.RecordFormat("u", "one", "pcm")
	if got := a.formats["u/one"]["pcm"].State; got != "served" {
		t.Fatal(got)
	}
	a.RecordFormat("u", "one", "unknown")
	if len(a.formats["u/one"]) != 1 {
		t.Fatal("unknown format recorded")
	}
}
