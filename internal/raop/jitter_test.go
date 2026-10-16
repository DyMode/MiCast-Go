package raop

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"micastgo/internal/playback"
)

func TestExpiredGapReleasesWholeMissingRunWithoutAnotherWait(t *testing.T) {
	for _, first := range []uint16{100, 65534} {
		q := playback.NewPCMQueue(context.Background())
		s := &session{rawPCM: true, frameLength: 1, queue: q, expected: first,
			pending: map[uint16][]byte{first + 3: {0, 1, 0, 2}, first + 4: {0, 3, 0, 4}},
			gapAt:   time.Now().Add(-time.Second)}
		s.drain()
		if s.expected != first+5 || len(s.pending) != 0 {
			t.Fatalf("one expired loss burst must not wait 400ms for each missing packet: expected=%d pending=%d", s.expected, len(s.pending))
		}
		q.Finish()
		pcm, err := io.ReadAll(q)
		if err != nil || !bytes.Equal(pcm, []byte{1, 0, 2, 0, 3, 0, 4, 0}) {
			t.Fatalf("loss recovery must emit only valid audio, without synthetic silence: %v %v", pcm, err)
		}
		q.Close()
	}
}

func TestReorderedGapRepairsBeforeDeadlineWithoutLossOrDuplicates(t *testing.T) {
	q := playback.NewPCMQueue(context.Background())
	defer q.Close()
	s := &session{rawPCM: true, frameLength: 1, queue: q, expected: 100, haveExpected: true,
		pending: map[uint16][]byte{103: {0, 4, 0, 4}}}
	s.drain()
	for _, sequence := range []uint16{102, 101, 100, 102} {
		packet := make([]byte, 16)
		packet[0] = 0x80
		binary.BigEndian.PutUint16(packet[2:4], sequence)
		packet[13], packet[15] = byte(sequence-99), byte(sequence-99)
		s.process(packet)
	}
	if s.expected != 104 || s.lostPackets != 0 || len(s.pending) != 0 {
		t.Fatal("reordered retransmissions were lost", s.expected, s.lostPackets)
	}
	q.Finish()
	pcm, err := io.ReadAll(q)
	want := []byte{1, 0, 1, 0, 2, 0, 2, 0, 3, 0, 3, 0, 4, 0, 4, 0}
	if err != nil || !bytes.Equal(pcm, want) {
		t.Fatal("decoded audio contains missing or duplicated frames", pcm, err)
	}
}

func TestRetransmitRequestsCoverBurstAndUseDistinctRequestSequences(t *testing.T) {
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	q := playback.NewPCMQueue(context.Background())
	defer q.Close()
	s := &session{queue: q, expected: 100, frameLength: 1, control: control,
		peerControl: peer.LocalAddr().(*net.UDPAddr), pending: map[uint16][]byte{103: {0, 1, 0, 2}}}
	var previous uint16
	for i := 0; i < 2; i++ {
		s.lastResend = time.Now().Add(-time.Second)
		s.drain()
		peer.SetReadDeadline(time.Now().Add(time.Second))
		packet := make([]byte, 8)
		if n, _, err := peer.ReadFromUDP(packet); err != nil || n != 8 {
			t.Fatal("no retransmit request", n, err)
		}
		sequence := binary.BigEndian.Uint16(packet[2:4])
		if binary.BigEndian.Uint16(packet[4:6]) != 100 || binary.BigEndian.Uint16(packet[6:8]) != 3 {
			t.Fatalf("request must cover all three missing packets: %v", packet)
		}
		if i > 0 && sequence == previous {
			t.Fatal("retransmit retries reused the same request sequence")
		}
		previous = sequence
	}
}
