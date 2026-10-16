package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"micastgo/internal/app"
	"micastgo/internal/testsupport"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFiniteMediaWaitsForSpeakerAndReusesCache(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	application, cloud := testsupport.NewApp(t, "wav")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var engine *Engine
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { engine.ServeHTTP(w, r) }))
	defer server.Close()
	dir := t.TempDir()
	engine, err = New(ctx, application, ffmpeg, server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	pcm := make([]byte, 44100*4/2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(i*31%16000))
	}
	header := wavHeader()
	binary.LittleEndian.PutUint32(header[4:], uint32(len(pcm)+36))
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	file := filepath.Join(dir, "cached-wave")
	if err = os.WriteFile(file, append(header, pcm...), 0600); err != nil {
		t.Fatal(err)
	}
	engine.cachePath = file
	engine.cacheURL = "http://192.168.1.250/cached.wav"
	if _, err = engine.PlayMedia(engine.cacheURL, Track{}, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case play := <-cloud.Plays:
		time.Sleep(800 * time.Millisecond)
		response, err := (&http.Client{Timeout: 8 * time.Second}).Get(play.URL)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) < 44 || !bytes.Equal(encoded[44:], pcm) {
			t.Fatalf("beginning/tail lost while waiting for speaker: got %d expected %d", len(encoded)-44, len(pcm))
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no playback command", engine.Status())
	}
	// The origin is intentionally not running. Seeking succeeds through the
	// same cached file, without making another network download.
	if err = engine.Seek(.1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cloud.Plays:
	case <-time.After(5 * time.Second):
		t.Fatal("seek did not reuse cached media", engine.Status())
	}
}

func TestLivePCMToHTTPAllFormats(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	for _, format := range []string{"mp3", "flac", "wav", "pcm"} {
		t.Run(format, func(t *testing.T) {
			application, cloud := testsupport.NewApp(t, format)
			var engine *Engine
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { engine.ServeHTTP(w, r) }))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			engine, err = New(ctx, application, ffmpeg, srv.URL, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			q := NewPCMQueue(ctx)
			_, err = engine.StartPCM(q, Track{})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				defer q.Finish()
				for frame := 0; frame < 180; frame++ {
					pcm := make([]byte, 352*4)
					for i := 0; i < len(pcm); i += 2 {
						binary.LittleEndian.PutUint16(pcm[i:], uint16((frame*352+i)*73%16000))
					}
					if !q.Push(pcm) {
						return
					}
					time.Sleep(8 * time.Millisecond)
				}
			}()
			var play testsupport.Play
			select {
			case play = <-cloud.Plays:
			case <-time.After(8 * time.Second):
				t.Fatal("no cloud playback command", engine.Status())
			}
			client := &http.Client{Timeout: 8 * time.Second}
			resp, err := client.Get(play.URL)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || len(encoded) == 0 {
				t.Fatal("no audio", err)
			}
			command := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-f", "s16le", "-acodec", "pcm_s16le", "pipe:1")
			command.Stdin = bytes.NewReader(encoded)
			pcm, err := command.Output()
			if err != nil || len(pcm) < 1000 {
				t.Fatalf("HTTP output could not decode: format=%s bytes=%d decoded=%d err=%v", format, len(encoded), len(pcm), err)
			}
			if status := engine.Status(); status.Streams["speaker"].Bytes == 0 {
				t.Fatal("pull accounting missing")
			}
			off := false
			if err = application.UpdateSpeaker(ctx, "speaker", app.SpeakerPatch{WholeHouse: &off}); err != nil {
				t.Fatal(err)
			}
			denied, err := client.Get(play.URL)
			if err != nil {
				t.Fatal(err)
			}
			denied.Body.Close()
			if denied.StatusCode != 404 {
				t.Fatal("disabled target URL accepted")
			}
		})
	}
}
func TestPCMQueueFinishDrainsWithoutLosingTail(t *testing.T) {
	q := NewPCMQueue(context.Background())
	q.Push([]byte("one"))
	q.Push([]byte("two"))
	q.Finish()
	result, err := io.ReadAll(q)
	if err != nil || string(result) != "onetwo" {
		t.Fatal(string(result), err)
	}
}
func TestRollingHubIsBoundedAndHeadersMayBeSplit(t *testing.T) {
	h := newHub("wav")
	for i := 0; i < 100; i++ {
		h.Write(make([]byte, 65536))
	}
	if h.bytes > 4*1024*1024 {
		t.Fatal("unbounded history")
	}
	flac := newHub("flac")
	header := append([]byte("fLaC"), 0x80, 0, 0, 3, 1, 2, 3)
	for _, b := range header {
		if _, err := flac.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	flac.Write([]byte("audio"))
	if !bytes.Equal(flac.header, header) {
		t.Fatal("split FLAC metadata lost")
	}
}
func TestMediaRejectsLocalServiceAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transport := mediaTransport()
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:1/private", nil)
	_, err := (&http.Client{Transport: transport}).Do(req)
	if err == nil {
		t.Fatal("loopback media accepted")
	}
	if validMediaURL("file:///etc/passwd") || validMediaURL("http://user:secret@example.test/a") {
		t.Fatal("unsafe URL accepted")
	}
}
func TestWAVHeaderIsStreamingPCM(t *testing.T) {
	header := wavHeader()
	if len(header) != 44 || string(header[:4]) != "RIFF" || binary.LittleEndian.Uint32(header[24:]) != 44100 {
		t.Fatal("invalid WAV header")
	}
}

func TestNaturalCompletionIsDistinctFromUserStop(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires FFmpeg")
	}
	a, _ := testsupport.NewApp(t, "wav")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, err := New(ctx, a, ffmpeg, "http://127.0.0.1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := e.newSession("dlna", Track{}, 0)
	e.stop(s.id, true)
	if !e.Status().Finished || e.Status().State != "stopped" {
		t.Fatal("natural completion missing")
	}
	e.Stop(s.id)
	if e.Status().Finished {
		t.Fatal("manual stop treated as queue completion")
	}
}
