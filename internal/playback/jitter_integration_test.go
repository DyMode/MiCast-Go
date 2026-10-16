package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"micastgo/internal/testsupport"
)

func TestBurstyLiveHTTPPreservesPCMThroughRepeatedSourceGaps(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	application, cloud := testsupport.NewApp(t, "wav")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var engine *Engine
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { engine.ServeHTTP(w, r) }))
	defer srv.Close()
	engine, err = New(ctx, application, ffmpeg, srv.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	q := NewPCMQueue(ctx)
	if _, err = engine.StartPCM(q, Track{}); err != nil {
		t.Fatal(err)
	}
	const packets = 240
	const packetBytes = 882 * 4 // 20 ms stereo PCM
	want := make([]byte, packets*packetBytes)
	for i := 0; i < len(want); i += 2 {
		binary.LittleEndian.PutUint16(want[i:], uint16(i*17))
	}
	producerDone := make(chan bool, 1)
	go func() {
		defer q.Finish()
		started := time.Now()
		for i := 0; i < packets; i++ {
			due := started.Add(time.Duration(i) * 20 * time.Millisecond)
			// A delayed source catches up in a burst, preserving its sample clock.
			if i > 0 && i%40 == 0 {
				due = due.Add(180 * time.Millisecond)
			}
			if wait := time.Until(due); wait > 0 {
				time.Sleep(wait)
			}
			if !q.Push(want[i*packetBytes : (i+1)*packetBytes]) {
				producerDone <- false
				return
			}
		}
		producerDone <- true
	}()
	select {
	case play := <-cloud.Plays:
		response, err := (&http.Client{Timeout: 12 * time.Second}).Get(play.URL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || len(got) < 44 || !bytes.Equal(got[44:], want) {
			t.Fatalf("burst recovery changed audio: got=%d want=%d err=%v", len(got)-44, len(want), err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no playback command")
	}
	if !<-producerDone {
		t.Fatal("live input overflowed")
	}
	status := engine.Status()
	if status.InputDroppedBytes != 0 || status.EncodingDroppedBytes != 0 || status.Streams["speaker"].HistorySkips != 0 {
		t.Fatal("healthy bursty source lost audio", status)
	}
}
