package raop

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/grandcat/zeroconf"
	alac "github.com/mycophonic/saprobe-alac"
	"io"
	"log/slog"
	"micastgo/internal/netutil"
	"micastgo/internal/playback"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	listener net.Listener
	service  *zeroconf.Server
	engine   *playback.Engine
	key      *rsa.PrivateKey
	mac      []byte
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	clients  map[net.Conn]bool
	wg       sync.WaitGroup
}

func Start(ctx context.Context, engine *playback.Engine, name, host, address string, announce bool, identity ...string) (*Server, error) {
	key, err := airportKey()
	if err != nil {
		return nil, err
	}
	listener, err := netutil.ListenTCP("tcp4", address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	id := ""
	if len(identity) > 0 {
		id = "/" + identity[0]
	}
	hash := sha256.Sum256([]byte("MiCast Go/" + host + id))
	mac := append([]byte(nil), hash[:6]...)
	mac[0] = (mac[0] | 2) & 254
	s := &Server{listener: listener, engine: engine, key: key, mac: mac, ctx: ctx, cancel: cancel, clients: map[net.Conn]bool{}}
	if announce {
		port := listener.Addr().(*net.TCPAddr).Port
		instance := strings.ToUpper(hex.EncodeToString(mac)) + "@" + name
		s.service, err = zeroconf.RegisterProxy(instance, "_raop._tcp", "local.", port, "micast-go-"+hex.EncodeToString(mac)+".local.", []string{host}, []string{"txtvers=1", "ch=2", "cn=0,1", "da=true", "et=0,1", "am=MiCastGo1,1", "md=0,1,2", "pw=false", "sr=44100", "ss=16", "sv=false", "sf=0x4", "tp=UDP", "vn=3", "vs=105.1"}, nil)
		if err != nil {
			listener.Close()
			cancel()
			return nil, err
		}
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}
func (s *Server) Addr() net.Addr { return s.listener.Addr() }
func (s *Server) Close() {
	s.cancel()
	s.listener.Close()
	s.mu.Lock()
	for c := range s.clients {
		c.Close()
	}
	s.mu.Unlock()
	if s.service != nil {
		s.service.Shutdown()
	}
	s.wg.Wait()
}
func (s *Server) serve() {
	defer s.wg.Done()
	slots := make(chan struct{}, 8)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		s.mu.Lock()
		s.clients[conn] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-slots; conn.Close(); s.mu.Lock(); delete(s.clients, conn); s.mu.Unlock() }()
			s.client(conn)
		}()
	}
}

type request struct {
	method  string
	headers map[string]string
	body    []byte
}

func readRequest(r *bufio.Reader) (request, error) {
	result := request{headers: map[string]string{}}
	line, err := r.ReadString('\n')
	if err != nil {
		return result, err
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "RTSP/") {
		return result, errors.New("invalid RTSP request")
	}
	result.method = parts[0]
	size := len(line)
	for {
		line, err = r.ReadString('\n')
		if err != nil {
			return result, err
		}
		size += len(line)
		if size > 32768 {
			return result, errors.New("RTSP headers too large")
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return result, errors.New("invalid RTSP header")
		}
		result.headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	length := 0
	if raw := result.headers["content-length"]; raw != "" {
		length, err = strconv.Atoi(raw)
		if err != nil || length < 0 || length > 4*1024*1024 {
			return result, errors.New("invalid RTSP body")
		}
	}
	result.body = make([]byte, length)
	_, err = io.ReadFull(r, result.body)
	return result, err
}
func writeResponse(c net.Conn, r request, status int, headers map[string]string, body []byte) error {
	reason := "OK"
	if status != 200 {
		reason = "Bad Request"
	}
	fmt.Fprintf(c, "RTSP/1.0 %d %s\r\nCSeq: %s\r\nServer: AirTunes/105.1\r\nAudio-Jack-Status: connected; type=analog\r\n", status, reason, r.headers["cseq"])
	for k, v := range headers {
		fmt.Fprintf(c, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(c, "Content-Length: %d\r\n\r\n", len(body))
	_, err := c.Write(body)
	return err
}
func (s *Server) client(conn net.Conn) {
	sess := &session{server: s, conn: conn, pending: map[uint16][]byte{}}
	defer sess.close()
	reader := bufio.NewReaderSize(conn, 32768)
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		r, err := readRequest(reader)
		if err != nil {
			return
		}
		headers := map[string]string{}
		status := 200
		var body []byte
		if challenge := r.headers["apple-challenge"]; challenge != "" {
			response, err := appleResponse(s.key, challenge, conn.LocalAddr().(*net.TCPAddr).IP, s.mac)
			if err != nil {
				return
			}
			headers["Apple-Response"] = response
		}
		switch r.method {
		case "OPTIONS":
			headers["Public"] = "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER"
		case "ANNOUNCE":
			if err := sess.configure(string(r.body)); err != nil {
				status = 400
				slog.Warn("AirPlay 格式协商失败", "reason", err.Error())
			}
		case "SETUP":
			if err := sess.open(r.headers["transport"]); err != nil {
				status = 400
			} else {
				headers["Transport"] = fmt.Sprintf("RTP/AVP/UDP;unicast;mode=record;server_port=%d;control_port=%d;timing_port=%d", sess.audio.LocalAddr().(*net.UDPAddr).Port, sess.control.LocalAddr().(*net.UDPAddr).Port, sess.timing.LocalAddr().(*net.UDPAddr).Port)
				headers["Session"] = "1"
			}
		case "RECORD":
			if err := sess.record(); err != nil {
				status = 400
			}
			headers["Audio-Latency"] = "11025"
		case "FLUSH":
			sess.mu.Lock()
			sess.pending = map[uint16][]byte{}
			sess.haveExpected = false
			sess.gapAt = time.Time{}
			sess.lastResend = time.Time{}
			sess.resends = 0
			sess.mu.Unlock()
			// Like MiCast, flush transport state only. Recreating the playback
			// session here resets the encoder and makes every speaker reconnect.
		case "PAUSE":
			ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			_ = s.engine.Pause(ctx)
			cancel()
		case "SET_PARAMETER":
			if strings.Contains(r.headers["content-type"], "x-dmap-tagged") {
				sess.track = parseDMAP(r.body)
				s.engine.Metadata(sess.owner, sess.track)
			} else if strings.Contains(r.headers["content-type"], "text/parameters") {
				for _, line := range strings.Split(string(r.body), "\n") {
					if raw, ok := strings.CutPrefix(strings.TrimSpace(line), "volume:"); ok {
						db, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
						if err == nil {
							percent := int((db + 30) / 30 * 100)
							if db <= -144 {
								percent = 0
							}
							percent = max(0, min(100, percent))
							go func() {
								ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
								defer cancel()
								_ = s.engine.Volume(ctx, percent)
							}()
						}
					}
				}
			}
		case "GET_PARAMETER":
			headers["Content-Type"] = "text/parameters"
			if strings.Contains(string(r.body), "volume") {
				body = []byte("volume: -10.000000\r\n")
			}
		case "TEARDOWN":
			_ = writeResponse(conn, r, 200, headers, nil)
			return
		default:
			status = 501
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := writeResponse(conn, r, status, headers, body); err != nil {
			return
		}
	}
}

type session struct {
	server                     *Server
	conn                       net.Conn
	mu                         sync.Mutex
	decoder                    *alac.PacketDecoder
	block                      cipher.Block
	iv                         []byte
	rawPCM                     bool
	frameLength                int
	audio, control, timing     *net.UDPConn
	peerControl, peerTiming    *net.UDPAddr
	ctx                        context.Context
	cancel                     context.CancelFunc
	queue                      *playback.PCMQueue
	owner                      string
	track                      playback.Track
	pending                    map[uint16][]byte
	expected                   uint16
	haveExpected               bool
	gapAt, lastRTP, lastResend time.Time
	resends                    int
	resendSequence             uint16
	lostPackets                uint64
	lostBursts                 uint64
	wg                         sync.WaitGroup
}

func (s *session) configure(sdp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decoder = nil
	s.rawPCM = false
	s.block = nil
	s.iv = nil
	values := map[string]string{}
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=") {
			key, value, ok := strings.Cut(line[2:], ":")
			if ok {
				values[key] = value
			}
		}
	}
	if encoded := values["rsaaeskey"]; encoded != "" {
		key, err := decryptKey(s.server.key, encoded)
		if err != nil || len(key) != 16 {
			return errors.New("invalid AirPlay encryption key")
		}
		s.block, err = aes.NewCipher(key)
		if err != nil {
			return err
		}
		s.iv, err = decode64(values["aesiv"])
		if err != nil || len(s.iv) != 16 {
			return errors.New("invalid AirPlay IV")
		}
	}
	if strings.Contains(values["rtpmap"], "L16/44100/2") {
		s.rawPCM = true
		s.frameLength = 352
		return nil
	}
	fields := strings.Fields(values["fmtp"])
	if len(fields) == 12 {
		fields = fields[1:]
	}
	if len(fields) != 11 {
		return errors.New("unsupported ALAC configuration")
	}
	f := make([]int, 11)
	for i, raw := range fields {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		f[i] = n
	}
	if f[0] < 1 || f[0] > 4096 || f[2] != 16 || f[6] != 2 || f[10] != 44100 || f[3] < 0 || f[3] > 255 || f[4] < 0 || f[4] > 255 || f[5] < 0 || f[5] > 31 || f[7] < 0 || f[7] > 65535 {
		return errors.New("only 44.1 kHz stereo 16-bit ALAC is supported")
	}
	decoder, err := alac.NewPacketDecoder(alac.PacketConfig{FrameLength: uint32(f[0]), BitDepth: 16, PB: uint8(f[3]), MB: uint8(f[4]), KB: uint8(f[5]), NumChannels: 2, MaxRun: uint16(f[7]), MaxFrameBytes: uint32(f[8]), AvgBitRate: uint32(f[9]), SampleRate: 44100})
	if err != nil {
		return err
	}
	s.decoder = decoder
	s.frameLength = f[0]
	return nil
}
func transportPort(value, key string) int {
	for _, part := range strings.Split(value, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == key {
			n, _ := strconv.Atoi(v)
			if n > 0 && n <= 65535 {
				return n
			}
		}
	}
	return 0
}
func (s *session) open(transport string) error {
	if s.decoder == nil && !s.rawPCM {
		return errors.New("ANNOUNCE required")
	}
	s.closeUDP()
	s.ctx, s.cancel = context.WithCancel(s.server.ctx)
	var err error
	for _, target := range []**net.UDPConn{&s.audio, &s.control, &s.timing} {
		*target, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
		if err != nil {
			s.closeUDP()
			return err
		}
		// Leave room for a Wi-Fi burst and retransmissions while ALAC is decoded.
		if err := (*target).SetReadBuffer(2 * 1024 * 1024); err != nil {
			slog.Warn("AirPlay 接收缓冲设置失败", "error", err)
		}
	}
	peer := s.conn.RemoteAddr().(*net.TCPAddr).IP
	s.peerControl = &net.UDPAddr{IP: peer, Port: transportPort(transport, "control_port")}
	s.peerTiming = &net.UDPAddr{IP: peer, Port: transportPort(transport, "timing_port")}
	s.lastRTP = time.Now()
	s.wg.Add(4)
	go s.receive(s.audio, false, false)
	go s.receive(s.control, true, false)
	go s.receive(s.timing, false, true)
	go s.watch()
	return nil
}
func (s *session) record() error {
	if s.audio == nil {
		return errors.New("SETUP required")
	}
	q := playback.NewPCMQueue(s.ctx)
	id, err := s.server.engine.StartPCM(q, s.track)
	if err != nil {
		q.Close()
		return err
	}
	s.mu.Lock()
	old := s.queue
	s.queue = q
	s.owner = id
	s.pending = map[uint16][]byte{}
	s.haveExpected = false
	s.gapAt = time.Time{}
	s.lastResend = time.Time{}
	s.resends = 0
	s.lastRTP = time.Now()
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	s.server.mu.Lock()
	for conn := range s.server.clients {
		if conn != s.conn {
			conn.Close()
		}
	}
	s.server.mu.Unlock()
	return nil
}
func (s *session) closeUDP() {
	if s.cancel != nil {
		s.cancel()
	}
	for _, socket := range []*net.UDPConn{s.audio, s.control, s.timing} {
		if socket != nil {
			socket.Close()
		}
	}
	s.wg.Wait()
	s.audio = nil
	s.control = nil
	s.timing = nil
}
func (s *session) close() {
	s.closeUDP()
	if s.queue != nil {
		s.queue.Close()
	}
	if s.owner != "" {
		s.server.engine.Stop(s.owner)
	}
}
func (s *session) receive(socket *net.UDPConn, control, timing bool) {
	defer s.wg.Done()
	buffer := make([]byte, 65536)
	peer := s.conn.RemoteAddr().(*net.TCPAddr).IP
	for {
		n, from, err := socket.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if !from.IP.Equal(peer) {
			continue
		}
		packet := buffer[:n]
		if timing {
			if reply := timingReply(packet, time.Now()); reply != nil {
				socket.WriteToUDP(reply, from)
			}
			continue
		}
		if control {
			if n < 16 || packet[1]&127 != 0x56 {
				continue
			}
			packet = packet[4:]
		}
		s.process(packet)
	}
}
func timingReply(packet []byte, now time.Time) []byte {
	if len(packet) < 32 || packet[1]&127 != 0x52 {
		return nil
	}
	reply := make([]byte, 32)
	reply[0] = 0x80
	reply[1] = 0xd3
	copy(reply[8:16], packet[24:32])
	stamp := ntpStamp(now)
	copy(reply[16:24], stamp)
	copy(reply[24:], stamp)
	return reply
}
func ntpStamp(now time.Time) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(now.Unix()+2208988800))
	binary.BigEndian.PutUint32(b[4:], uint32(uint64(now.Nanosecond())*(1<<32)/1000000000))
	return b
}
func (s *session) watch() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			idle := s.queue != nil && time.Since(s.lastRTP) > 15*time.Second
			s.drain()
			s.mu.Unlock()
			if idle {
				s.conn.Close()
				return
			}
			if s.peerTiming.Port > 0 {
				packet := make([]byte, 32)
				packet[0] = 0x80
				packet[1] = 0xd2
				copy(packet[24:], ntpStamp(time.Now()))
				s.timing.WriteToUDP(packet, s.peerTiming)
			}
		}
	}
}
func (s *session) process(packet []byte) {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return
	}
	offset := 12 + int(packet[0]&15)*4
	if offset > len(packet) {
		return
	}
	if packet[0]&16 != 0 {
		if offset+4 > len(packet) {
			return
		}
		offset += 4 + int(binary.BigEndian.Uint16(packet[offset+2:]))*4
	}
	if offset >= len(packet) {
		return
	}
	payload := append([]byte(nil), packet[offset:]...)
	if packet[0]&32 != 0 {
		n := int(payload[len(payload)-1])
		if n == 0 || n > len(payload) {
			return
		}
		payload = payload[:len(payload)-n]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue == nil {
		return
	}
	s.lastRTP = time.Now()
	if s.block != nil {
		n := len(payload) / 16 * 16
		cipher.NewCBCDecrypter(s.block, s.iv).CryptBlocks(payload[:n], payload[:n])
	}
	sequence := binary.BigEndian.Uint16(packet[2:4])
	if !s.haveExpected {
		s.expected = sequence
		s.haveExpected = true
	}
	distance := uint16(sequence - s.expected)
	if distance >= 32768 {
		return
	}
	if _, exists := s.pending[sequence]; !exists {
		s.pending[sequence] = payload
	}
	s.drain()
}
func (s *session) drain() {
	for len(s.pending) > 0 {
		payload, ok := s.pending[s.expected]
		if !ok {
			// Find the next real packet in modular sequence order. One loss
			// burst gets one resend window, not a fresh wait for every packet.
			missing := uint16(0xffff)
			for sequence := range s.pending {
				distance := uint16(sequence - s.expected)
				if distance > 0 && distance < 32768 && distance < missing {
					missing = distance
				}
			}
			if missing == 0xffff {
				s.pending = map[uint16][]byte{}
				return
			}
			if s.gapAt.IsZero() {
				s.gapAt = time.Now()
				s.resends = 0
			}
			if s.control != nil && s.peerControl != nil && s.peerControl.Port > 0 && time.Since(s.lastResend) > 60*time.Millisecond && s.resends < 6 {
				packet := []byte{0x80, 0xd5, 0, 1, 0, 0, 0, 1}
				s.resendSequence++
				binary.BigEndian.PutUint16(packet[2:4], s.resendSequence)
				binary.BigEndian.PutUint16(packet[4:6], s.expected)
				binary.BigEndian.PutUint16(packet[6:8], min(missing, 48))
				s.control.WriteToUDP(packet, s.peerControl)
				s.lastResend = time.Now()
				s.resends++
			}
			if len(s.pending) <= 48 && time.Since(s.gapAt) < 400*time.Millisecond {
				return
			}
			s.lostPackets += uint64(missing)
			s.lostBursts++
			if s.lostBursts <= 3 || s.lostBursts%100 == 0 {
				slog.Warn("AirPlay 缺包补发超时或超出等待窗口", "missing_packets", missing, "total_missing", s.lostPackets)
			}
			// Match MiCast: advance to real audio. Do not manufacture PCM for
			// every lost packet; repeated silence fills consume the jitter margin.
			s.expected += missing
			s.gapAt = time.Time{}
			continue
		}
		delete(s.pending, s.expected)
		s.expected++
		s.gapAt = time.Time{}
		var pcm []byte
		if s.rawPCM {
			pcm = payload
			for i := 0; i+1 < len(pcm); i += 2 {
				pcm[i], pcm[i+1] = pcm[i+1], pcm[i]
			}
		} else {
			pcm, _ = safeDecode(s.decoder, payload)
		}
		if len(pcm) > 0 {
			s.queue.Push(pcm)
		}
	}
}
func safeDecode(decoder *alac.PacketDecoder, packet []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("invalid ALAC packet")
		}
	}()
	return decoder.DecodePacket(packet)
}
func parseDMAP(data []byte) playback.Track {
	values := map[string]string{}
	var walk func([]byte, int)
	walk = func(data []byte, depth int) {
		if depth > 8 {
			return
		}
		for len(data) >= 8 {
			tag := string(data[:4])
			n := int(binary.BigEndian.Uint32(data[4:8]))
			data = data[8:]
			if n > len(data) {
				return
			}
			payload := data[:n]
			data = data[n:]
			switch tag {
			case "mdcl", "mlit", "msrv", "mlcl":
				walk(payload, depth+1)
			case "minm", "asar", "asal":
				if len(payload) <= 2048 {
					values[tag] = strings.TrimSpace(string(payload))
				}
			}
		}
	}
	walk(data, 0)
	title, artist := values["minm"], values["asar"]
	for _, sep := range []string{" - ", " · "} {
		if prefix, suffix, ok := strings.Cut(title, sep); ok && artist != "" && (strings.Contains(artist, suffix) || strings.Contains(suffix, artist)) {
			title = prefix
			break
		}
	}
	return playback.Track{Title: title, Artist: artist, Album: values["asal"]}
}
