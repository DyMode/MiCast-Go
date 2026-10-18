package raop

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"micastgo/internal/playback"
	"micastgo/internal/testsupport"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompressedALACFixtureMatchesOriginalPCM(t *testing.T) {
	packet, err := os.ReadFile("testdata/sine.alac")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/sine.s16le")
	if err != nil {
		t.Fatal(err)
	}
	fmtp, err := os.ReadFile("testdata/sine.fmtp")
	if err != nil {
		t.Fatal(err)
	}
	sess := &session{}
	if err = sess.configure("a=fmtp:" + strings.TrimSpace(string(fmtp)) + "\r\n"); err != nil {
		t.Fatal(err)
	}
	got, err := safeDecode(sess.decoder, packet)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("compressed ALAC mismatch: got=%d want=%d err=%v", len(got), len(want), err)
	}
}

func TestAppleChallengeAndSessionKey(t *testing.T) {
	key, err := airportKey()
	if err != nil {
		t.Fatal(err)
	}
	challenge := bytes.Repeat([]byte{7}, 16)
	mac := []byte{2, 3, 4, 5, 6, 7}
	response, err := appleResponse(key, base64.RawStdEncoding.EncodeToString(challenge), net.ParseIP("192.168.1.20"), mac)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decode64(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != key.Size() {
		t.Fatal("invalid signature size")
	}
	secret := bytes.Repeat([]byte{5}, 16)
	encrypted, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decryptKey(key, base64.RawStdEncoding.EncodeToString(encrypted))
	if err != nil || !bytes.Equal(decoded, secret) {
		t.Fatal("session key did not roundtrip", err)
	}
}
func TestRTSPParserHandlesPipeliningAndInvalidLengths(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\nSET_PARAMETER * RTSP/1.0\r\nCSeq: 2\r\nContent-Length: 3\r\n\r\nabc"))
	one, err := readRequest(reader)
	if err != nil || one.method != "OPTIONS" {
		t.Fatal(err)
	}
	two, err := readRequest(reader)
	if err != nil || string(two.body) != "abc" {
		t.Fatal(err)
	}
	_, err = readRequest(bufio.NewReader(strings.NewReader("ANNOUNCE * RTSP/1.0\r\nContent-Length: -1\r\n\r\n")))
	if err == nil {
		t.Fatal("negative length accepted")
	}
}
func TestALACUncompressedPacketDecodesToPCM(t *testing.T) {
	sess := &session{}
	if err := sess.configure("a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"); err != nil {
		t.Fatal(err)
	}
	var bits []byte
	put := func(v uint64, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, byte(v>>i)&1)
		}
	}
	put(1, 3)
	put(0, 4)
	put(0, 12)
	put(9, 4)
	put(3, 32)
	samples := []int16{1000, -1000, 2000, -2000, 3000, -3000}
	for _, sample := range samples {
		put(uint64(uint16(sample)), 16)
	}
	put(7, 3)
	packet := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		packet[i/8] |= b << uint(7-i%8)
	}
	pcm, err := safeDecode(sess.decoder, packet)
	if err != nil || len(pcm) != len(samples)*2 {
		t.Fatal("ALAC decode failed", err, len(pcm))
	}
	for i, sample := range samples {
		if int16(binary.LittleEndian.Uint16(pcm[i*2:])) != sample {
			t.Fatal("sample mismatch", i)
		}
	}
}
func TestTimingReply(t *testing.T) {
	packet := make([]byte, 32)
	packet[1] = 0xd2
	copy(packet[24:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	reply := timingReply(packet, time.Unix(0, 0))
	if len(reply) != 32 || reply[1] != 0xd3 || !bytes.Equal(reply[8:16], packet[24:]) || binary.BigEndian.Uint32(reply[16:]) != 2208988800 {
		t.Fatal("invalid timing reply")
	}
}

func TestRepeatedRecordReusesStreamAndSupersededRecordCannotReclaim(t *testing.T) {
	a, _ := testsupport.NewApp(t, "wav")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e, err := playback.New(ctx, a, binary, "http://renderer", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	s := &session{server: &Server{engine: e}, ctx: ctx, audio: &net.UDPConn{}}
	if err := s.record(); err != nil {
		t.Fatal(err)
	}
	firstQueue, firstOwner := s.queue, s.owner
	if err := s.record(); err != nil || s.queue != firstQueue || s.owner != firstOwner {
		t.Fatal("repeated RECORD replaced the PCM stream", err)
	}
	q := playback.NewPCMQueue(ctx)
	newOwner, err := e.StartPCM(q, playback.Track{Title: "new sender"})
	if err != nil {
		t.Fatal(err)
	}
	if s.record() == nil || e.Status().SessionID != newOwner {
		t.Fatal("superseded connection reclaimed playback")
	}
}
func TestAirPlayRTSPAndUDPAudioReachHTTPStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("requires development FFmpeg")
	}
	application, cloud := testsupport.NewApp(t, "mp3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var engine *playback.Engine
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { engine.ServeHTTP(w, r) }))
	defer httpServer.Close()
	engine, err = playback.New(ctx, application, ffmpeg, httpServer.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	server, err := Start(ctx, engine, "Test", "127.0.0.1", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	cseq := 0
	send := func(method, headers, body string) map[string]string {
		t.Helper()
		cseq++
		conn.SetDeadline(time.Now().Add(8 * time.Second))
		fmt.Fprintf(conn, "%s rtsp://127.0.0.1/test RTSP/1.0\r\nCSeq: %d\r\n%sContent-Length: %d\r\n\r\n%s", method, cseq, headers, len(body), body)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "200") {
			t.Fatal("RTSP failure", method, line, err)
		}
		result := map[string]string{}
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\r\n" {
				break
			}
			k, v, _ := strings.Cut(strings.TrimSpace(line), ":")
			result[strings.ToLower(k)] = strings.TrimSpace(v)
		}
		return result
	}
	send("OPTIONS", "", "")
	send("ANNOUNCE", "Content-Type: application/sdp\r\n", "v=0\r\na=rtpmap:96 L16/44100/2\r\n")
	reply := send("SETUP", "Transport: RTP/AVP/UDP;unicast;mode=record;control_port=0;timing_port=0\r\n", "")
	port := transportPort(reply["transport"], "server_port")
	if port == 0 {
		t.Fatal("missing server UDP port")
	}
	send("RECORD", "", "")
	firstSession := engine.Status().SessionID
	send("FLUSH", "", "")
	send("RECORD", "", "")
	if after := engine.Status().SessionID; after != firstSession {
		t.Fatalf("track boundary restarted playback: %s -> %s", firstSession, after)
	}
	udp, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	feeding := make(chan struct{})
	go func() {
		defer close(feeding)
		for seq := 0; seq < 220; seq++ {
			packet := make([]byte, 12+352*4)
			packet[0] = 0x80
			packet[1] = 96
			binary.BigEndian.PutUint16(packet[2:], uint16(seq))
			binary.BigEndian.PutUint32(packet[4:], uint32(seq*352))
			for i := 12; i < len(packet); i += 2 {
				binary.BigEndian.PutUint16(packet[i:], uint16((seq*352+i)*47%16000))
			}
			udp.Write(packet)
			time.Sleep(8 * time.Millisecond)
		}
	}()
	var play testsupport.Play
	select {
	case play = <-cloud.Plays:
	case <-time.After(8 * time.Second):
		t.Fatal("no playback command", engine.Status())
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(play.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	response.Body.Close()
	if err != nil || len(data) < 1000 {
		t.Fatal("no audio stream", err, len(data))
	}
	decode := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-f", "s16le", "pipe:1")
	decode.Stdin = bytes.NewReader(data)
	pcm, err := decode.Output()
	if err != nil || len(pcm) == 0 {
		t.Fatal("AirPlay HTTP output failed to decode", err)
	}
	<-feeding
	beforeFlush := engine.Status().SessionID
	send("FLUSH", "", "")
	if after := engine.Status().SessionID; after != beforeFlush {
		t.Fatalf("FLUSH restarted the encoder and speaker stream: %s -> %s", beforeFlush, after)
	}
	send("TEARDOWN", "", "")
	if _, err = strconv.Atoi(reply["session"]); err != nil {
		t.Fatal("missing session")
	}
}
