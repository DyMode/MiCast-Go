package dlna

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"micastgo/internal/playback"
	"micastgo/internal/version"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var bootSequence atomic.Uint32

const renderer = "urn:schemas-upnp-org:device:MediaRenderer:1"

var serviceTypes = map[string]string{"AVTransport": "urn:schemas-upnp-org:service:AVTransport:1", "RenderingControl": "urn:schemas-upnp-org:service:RenderingControl:1", "ConnectionManager": "urn:schemas-upnp-org:service:ConnectionManager:1"}

const protocols = "http-get:*:audio/mpeg:*,http-get:*:audio/mp4:*,http-get:*:audio/aac:*,http-get:*:audio/flac:*,http-get:*:audio/wav:*"

type subscription struct {
	pending                     []string
	revision                    uint64
	retryAt                     time.Time
	failures                    int
	id, service, callback, peer string
	targets                     []callbackTarget
	expires                     time.Time
	sequence                    uint32
	last                        string
	busy                        bool
}
type Server struct {
	bootID        uint32
	discovery     *Discovery
	ownsDiscovery bool
	ctx           context.Context
	cancel        context.CancelFunc
	engine        *playback.Engine
	name, base    string
	uuid          string

	// mu protects subscriptions, trace and the advertised name. Protocol
	// state lives under stateMu and never shares a lock with engine calls.
	mu            sync.Mutex
	eventsWake    chan struct{}
	trace         []ProtocolEvent
	subscriptions map[string]*subscription
	pollAt        time.Time
	pollState     string
	pollDuration  string
	wg            sync.WaitGroup

	stateMu               sync.Mutex
	cast                  uint64
	trackID               uint64
	desired               string
	uri, metadata         string
	nextURI, nextMetadata string
	owner                 string
	engineTrackID         string
	positionBase          float64
	pendingStarts         int
	lastError             string
	track                 playback.Track
	nextTrack             playback.Track
	jobs                  chan job
}

func New(ctx context.Context, e *playback.Engine, name, base, identity string) *Server {
	ctx, cancel := context.WithCancel(ctx)
	digest := sha256.Sum256([]byte("micast-go/dlna/" + identity))
	h := hex.EncodeToString(digest[:16])
	uuid := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	s := &Server{bootID: uint32(time.Now().Unix()) + bootSequence.Add(1), ctx: ctx, cancel: cancel, engine: e, name: name, base: strings.TrimRight(base, "/"), uuid: "uuid:" + uuid, subscriptions: map[string]*subscription{}, eventsWake: make(chan struct{}, 1), jobs: make(chan job, 64), desired: "STOPPED"}
	s.wg.Add(3)
	go s.events()
	go s.lifecycle()
	go s.worker()
	return s
}
func (s *Server) StartDiscovery(iface *net.Interface) error {
	d, err := NewDiscovery(s.ctx, iface)
	if err != nil {
		return err
	}
	d.Add(s)
	s.ownsDiscovery = true
	return nil
}
func (s *Server) Close() {
	s.cancel()
	s.engine.Preempt(s.currentOwner())
	if s.discovery != nil {
		s.discovery.Remove(s)
		if s.ownsDiscovery {
			s.discovery.Close()
		}
	}
	s.wg.Wait()
}
func (s *Server) types() []string {
	return []string{"upnp:rootdevice", s.uuid, renderer, serviceTypes["AVTransport"], serviceTypes["RenderingControl"], serviceTypes["ConnectionManager"]}
}
func (s *Server) usn(t string) string {
	if t == s.uuid {
		return s.uuid
	}
	return s.uuid + "::" + t
}
func (s *Server) notifyOn(conn *net.UDPConn, kind string) {
	if conn == nil {
		return
	}
	for _, t := range s.types() {
		message := fmt.Sprintf("NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nCACHE-CONTROL: max-age=120\r\nLOCATION: %s/dlna/description.xml\r\nNT: %s\r\nNTS: %s\r\nSERVER: MiCast-Go/%s UPnP/1.0 DLNA/1.5\r\nUSN: %s\r\n\r\n", s.base, t, kind, version.Version, s.usn(t))
		message = strings.TrimSuffix(message, "\r\n\r\n") + fmt.Sprintf("\r\nBOOTID.UPNP.ORG: %d\r\nCONFIGID.UPNP.ORG: 1\r\n\r\n", s.bootID)
		conn.WriteToUDP([]byte(message), &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900})
	}
}
func escape(s string) string { var b bytes.Buffer; xml.EscapeText(&b, []byte(s)); return b.String() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/dlna/")
	if path == "description.xml" && (r.Method == "GET" || r.Method == "HEAD") {
		s.description(w)
		return
	}
	pieces := strings.Split(path, "/")
	if len(pieces) != 2 || serviceTypes[pieces[0]] == "" {
		http.NotFound(w, r)
		return
	}
	service, route := pieces[0], pieces[1]
	switch {
	case route == "scpd.xml" && r.Method == "GET":
		writeXML(w, http.StatusOK, scpd(service))
	case route == "control" && r.Method == "POST":
		s.control(w, r, service)
	case route == "event" && (r.Method == "SUBSCRIBE" || r.Method == "UNSUBSCRIBE"):
		s.subscribe(w, r, service)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) description(w http.ResponseWriter) {
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	baseURL, _ := url.Parse(s.base)
	prefix := strings.TrimRight(baseURL.Path, "/")
	var services strings.Builder
	for _, name := range []string{"AVTransport", "RenderingControl", "ConnectionManager"} {
		fmt.Fprintf(&services, "<service><serviceType>%s</serviceType><serviceId>urn:upnp-org:serviceId:%s</serviceId><SCPDURL>%s/dlna/%s/scpd.xml</SCPDURL><controlURL>%s/dlna/%s/control</controlURL><eventSubURL>%s/dlna/%s/event</eventSubURL></service>", serviceTypes[name], name, escape(prefix), name, escape(prefix), name, escape(prefix), name)
	}
	writeXML(w, http.StatusOK, fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><URLBase>%s/</URLBase><device><deviceType>%s</deviceType><friendlyName>%s</friendlyName><manufacturer>DyMode</manufacturer><modelName>MiCast Go</modelName><modelNumber>%s</modelNumber><dlna:X_DLNADOC>DMR-1.50</dlna:X_DLNADOC><UDN>%s</UDN><serviceList>%s</serviceList></device></root>`, escape(baseURL.Scheme+"://"+baseURL.Host), renderer, escape(name), version.Version, s.uuid, services.String()))
}

func (s *Server) Rename(name string) { s.mu.Lock(); s.name = name; s.mu.Unlock() }
func parseSOAP(r io.Reader) (string, map[string]string, error) {
	decoder := xml.NewDecoder(r)
	args := map[string]string{}
	action := ""
	inBody := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return action, args, nil
		}
		if err != nil {
			return "", nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "Body" {
				inBody = true
				continue
			}
			if !inBody {
				continue
			}
			if action == "" {
				action = value.Name.Local
				continue
			}
			var text string
			if err := decoder.DecodeElement(&text, &value); err != nil {
				return "", nil, err
			}
			args[value.Name.Local] = text
		}
	}
}

// XML responses use a finite Content-Length, including large DIDL metadata in
// GetPositionInfo and GetMediaInfo replies.
func writeXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func fault(w http.ResponseWriter, code int, text string) {
	writeXML(w, 500, fmt.Sprintf(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, escape(text)))
}
func (s *Server) control(w http.ResponseWriter, r *http.Request, service string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	action, args, err := parseSOAP(r.Body)
	if err != nil || action == "" {
		fault(w, 402, "Invalid Args")
		return
	}
	if args["InstanceID"] != "" && args["InstanceID"] != "0" {
		fault(w, 718, "Invalid InstanceID")
		return
	}
	if raw := r.Header.Get("SOAPAction"); raw != "" {
		expected := serviceTypes[service] + "#" + action
		if strings.Trim(raw, "\"") != expected {
			fault(w, 401, "Invalid Action")
			return
		}
	}
	if !hasAction(service, action) {
		fault(w, 401, "Invalid Action")
		return
	}
	args["_advance_next"] = r.Header.Get("X-MiCast-Play-Next")
	response, err := s.action(r.Context(), service, action, args)
	code := http.StatusOK
	if err != nil {
		code = http.StatusInternalServerError
	}
	s.recordProtocol(service, action, code, response)
	if err != nil {
		fault(w, 701, err.Error())
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%sResponse xmlns:u="%s">`, action, serviceTypes[service])
	for _, argument := range actions[service][action] {
		parts := strings.Split(argument, ":")
		if parts[1] != "out" {
			continue
		}
		key := parts[0]
		fmt.Fprintf(&body, "<%s>%s</%s>", key, escape(response[key]), key)
	}
	fmt.Fprintf(&body, "</u:%sResponse></s:Body></s:Envelope>", action)
	writeXML(w, http.StatusOK, body.String())
}
func (s *Server) action(ctx context.Context, service, action string, args map[string]string) (map[string]string, error) {
	switch service {
	case "AVTransport":
		return s.avTransport(ctx, action, args)
	case "RenderingControl":
		return s.renderingControl(ctx, action, args)
	default:
		return s.connectionManager(action)
	}
}

func (s *Server) avTransport(ctx context.Context, action string, args map[string]string) (map[string]string, error) {
	switch action {
	case "SetAVTransportURI":
		return s.setURI(args)
	case "SetNextAVTransportURI":
		return s.setNext(args)
	case "Next":
		return s.promote()
	case "Play":
		if args["Speed"] != "" && args["Speed"] != "1" {
			return nil, errors.New("Unsupported speed")
		}
		if args["_advance_next"] == "1" {
			return s.promote()
		}
		return s.play()
	case "Pause":
		return s.pause()
	case "Stop":
		return s.stop(), nil
	case "Seek":
		return s.seek(args)
	case "GetTransportInfo":
		snap := s.snapshot()
		return map[string]string{"CurrentTransportState": snap.State, "CurrentTransportStatus": snap.Status, "CurrentSpeed": "1"}, nil
	case "GetPositionInfo":
		snap := s.snapshot()
		track := "0"
		if snap.HasMedia {
			track = "1"
		}
		return map[string]string{"Track": track, "TrackDuration": formatTime(snap.Duration), "TrackMetaData": snap.Current.Metadata, "TrackURI": snap.Current.URI, "RelTime": formatTime(snap.Position), "AbsTime": formatTime(snap.Position), "RelCount": "0", "AbsCount": "0"}, nil
	case "GetMediaInfo":
		snap := s.snapshot()
		tracks := "0"
		if snap.HasMedia {
			tracks = "1"
		}
		return map[string]string{"NrTracks": tracks, "MediaDuration": formatTime(snap.Duration), "CurrentURI": snap.Current.URI, "CurrentURIMetaData": snap.Current.Metadata, "NextURI": snap.Next.URI, "NextURIMetaData": snap.Next.Metadata, "PlayMedium": "NETWORK", "RecordMedium": "NOT_IMPLEMENTED", "WriteStatus": "NOT_IMPLEMENTED"}, nil
	case "GetDeviceCapabilities":
		return map[string]string{"PlayMedia": "NETWORK", "RecMedia": "NOT_IMPLEMENTED", "RecQualityModes": "NOT_IMPLEMENTED"}, nil
	case "GetTransportSettings":
		return map[string]string{"PlayMode": "NORMAL", "RecQualityMode": "NOT_IMPLEMENTED"}, nil
	case "GetCurrentTransportActions":
		return map[string]string{"Actions": s.snapshot().Actions}, nil
	default:
		return nil, errors.New("Invalid Action")
	}
}

func (s *Server) renderingControl(ctx context.Context, action string, args map[string]string) (map[string]string, error) {
	owner := s.currentOwner()
	switch action {
	case "GetVolume":
		volume, _ := s.engine.VolumeState()
		return map[string]string{"CurrentVolume": strconv.Itoa(volume)}, nil
	case "SetVolume":
		volume, err := strconv.Atoi(args["DesiredVolume"])
		if err != nil || volume < 0 || volume > 100 {
			return nil, errors.New("Invalid volume")
		}
		if err = s.engine.ProtocolVolume(ctx, volume, "dlna", owner); err != nil {
			return nil, err
		}
	case "GetMute":
		_, muted := s.engine.VolumeState()
		if muted {
			return map[string]string{"CurrentMute": "1"}, nil
		}
		return map[string]string{"CurrentMute": "0"}, nil
	case "SetMute":
		mute := args["DesiredMute"] == "1" || strings.EqualFold(args["DesiredMute"], "true")
		if err := s.engine.ProtocolMute(ctx, mute, "dlna", owner); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("Invalid Action")
	}
	return map[string]string{}, nil
}

func (s *Server) connectionManager(action string) (map[string]string, error) {
	switch action {
	case "GetProtocolInfo":
		return map[string]string{"Source": "", "Sink": protocols}, nil
	case "GetCurrentConnectionIDs":
		return map[string]string{"ConnectionIDs": "0"}, nil
	case "GetCurrentConnectionInfo":
		return map[string]string{"RcsID": "0", "AVTransportID": "0", "ProtocolInfo": "", "PeerConnectionManager": "", "PeerConnectionID": "-1", "Direction": "Input", "Status": "OK"}, nil
	default:
		return nil, errors.New("Invalid Action")
	}
}

func (s *Server) currentOwner() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.owner
}

func (s *Server) setURI(args map[string]string) (map[string]string, error) {
	raw := strings.TrimSpace(args["CurrentURI"])
	if raw == "" || !validURI(raw) {
		return nil, errors.New("Invalid media URI")
	}
	meta := args["CurrentURIMetaData"]
	track := metadata(meta, raw)
	s.stateMu.Lock()
	oldOwner := s.owner
	s.cast++
	s.trackID++
	s.uri, s.metadata, s.track = raw, meta, track
	s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
	s.owner, s.engineTrackID = "", ""
	s.desired, s.lastError = "STOPPED", ""
	s.positionBase, s.pendingStarts = 0, 0
	s.stateMu.Unlock()
	// Stop the old audio immediately; the next Play establishes the new
	// source. This never waits on the cloud, so the SOAP reply stays fast.
	s.engine.Preempt(oldOwner)
	return map[string]string{"_duration": formatTime(track.Duration), "_meta": metaPresence(meta)}, nil
}

func (s *Server) setNext(args map[string]string) (map[string]string, error) {
	raw := strings.TrimSpace(args["NextURI"])
	if raw != "" && !validURI(raw) {
		return nil, errors.New("Invalid media URI")
	}
	meta := args["NextURIMetaData"]
	nextTrack := playback.Track{}
	if raw != "" {
		nextTrack = metadata(meta, raw)
	}
	s.stateMu.Lock()
	if raw == "" {
		s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
	} else {
		s.nextURI, s.nextMetadata, s.nextTrack = raw, meta, nextTrack
	}
	owner, cast := s.owner, s.cast
	s.stateMu.Unlock()
	if raw != "" && owner != "" {
		s.enqueue(job{kind: jobPrefetch, cast: cast, owner: owner, uri: raw})
	}
	s.advanceLocked(s.engine.Status())
	return map[string]string{"_duration": formatTime(nextTrack.Duration), "_meta": metaPresence(meta)}, nil
}

func (s *Server) play() (map[string]string, error) {
	s.stateMu.Lock()
	status := s.engine.Status()
	if s.uri == "" {
		s.stateMu.Unlock()
		return nil, errors.New("No media")
	}
	owner := s.owner
	ours := owner != "" && status.SessionID == owner && status.Protocol == "dlna"
	// A controller may send Play after the media already ended while its
	// pre-supplied Next is still queued. Promote instead of replaying.
	if ours && status.Finished && s.nextURI != "" {
		s.stateMu.Unlock()
		return s.promote()
	}
	if s.pendingStarts > 0 && s.desired == "PLAYING" {
		s.stateMu.Unlock()
		return map[string]string{}, nil
	}
	switch {
	case ours && status.State == "playing":
		s.desired = "PLAYING"
		s.stateMu.Unlock()
		return map[string]string{}, nil
	case ours && status.State == "paused":
		s.desired = "PLAYING"
		cast, track := s.cast, s.trackID
		s.stateMu.Unlock()
		s.enqueue(job{kind: jobResume, cast: cast, track: track, owner: owner})
		return map[string]string{}, nil
	case ours && (status.State == "transitioning" || status.State == "waiting_for_speakers"):
		s.desired = "PLAYING"
		s.stateMu.Unlock()
		return map[string]string{}, nil
	}
	uri, track, cast, trackID := s.uri, s.track, s.cast, s.trackID
	position := s.positionBase
	startOwner := ""
	if ours {
		startOwner = owner
	}
	s.desired = "PLAYING"
	s.owner, s.engineTrackID = "", ""
	s.pendingStarts++
	s.stateMu.Unlock()
	s.submit(job{kind: jobStart, cast: cast, track: trackID, seq: s.engine.Sequence(), owner: startOwner, uri: uri, info: track, position: position})
	return map[string]string{}, nil
}

func (s *Server) pause() (map[string]string, error) {
	s.stateMu.Lock()
	status := s.engine.Status()
	owner := s.owner
	if owner == "" || status.SessionID != owner || status.Protocol != "dlna" {
		s.stateMu.Unlock()
		return nil, errors.New("No active media")
	}
	s.desired = "PAUSED_PLAYBACK"
	cast, track := s.cast, s.trackID
	s.stateMu.Unlock()
	if status.State == "paused" {
		return map[string]string{}, nil
	}
	s.enqueue(job{kind: jobPause, cast: cast, track: track, owner: owner})
	return map[string]string{}, nil
}

func (s *Server) stop() map[string]string {
	s.stateMu.Lock()
	owner := s.owner
	s.cast++
	s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
	s.desired, s.lastError = "STOPPED", ""
	s.owner, s.engineTrackID = "", ""
	s.positionBase, s.pendingStarts = 0, 0
	s.stateMu.Unlock()
	s.engine.Preempt(owner)
	return map[string]string{}
}

func (s *Server) seek(args map[string]string) (map[string]string, error) {
	if args["Unit"] != "REL_TIME" {
		return nil, errors.New("Seek not available")
	}
	position, err := parseTime(args["Target"])
	if err != nil {
		return nil, err
	}
	s.stateMu.Lock()
	status := s.engine.Status()
	owner := s.owner
	if owner == "" || status.SessionID != owner || status.Protocol != "dlna" {
		s.stateMu.Unlock()
		return nil, errors.New("Seek not available")
	}
	s.positionBase = position
	cast, track := s.cast, s.trackID
	s.stateMu.Unlock()
	s.enqueue(job{kind: jobSeek, cast: cast, track: track, owner: owner, position: position})
	return map[string]string{}, nil
}

// promote starts the queued track immediately, used by the explicit Next
// action and by Play when a controller asks for the next item.
func (s *Server) promote() (map[string]string, error) {
	s.stateMu.Lock()
	status := s.engine.Status()
	if s.nextURI == "" {
		s.stateMu.Unlock()
		return nil, errors.New("No queued media URI")
	}
	owner := s.owner
	uri, meta, track := s.nextURI, s.nextMetadata, s.nextTrack
	s.trackID++
	s.uri, s.metadata, s.track = uri, meta, track
	s.nextURI, s.nextMetadata, s.nextTrack = "", "", playback.Track{}
	s.owner, s.engineTrackID = "", ""
	s.desired, s.positionBase = "PLAYING", 0
	s.pendingStarts++
	j := job{kind: jobStart, cast: s.cast, track: s.trackID, seq: s.engine.Sequence(), owner: owner, uri: uri, info: track}
	if owner != "" && status.Finished && status.SessionID == owner && status.Protocol == "dlna" {
		j.kind, j.continuation = jobAdvance, true
	}
	s.stateMu.Unlock()
	s.submit(j)
	return map[string]string{"_duration": formatTime(track.Duration), "_meta": metaPresence(meta)}, nil
}

// Polling and LastChange events describe the same snapshot. A natural end
// remains STOPPED until the controller provides more media; subscription
// expiry or silence from the controller never implies Stop.
func (s *Server) transportState(status playback.Status) string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.stateLocked(status)
}
func validURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || (!ip.IsLoopback() && !ip.IsLinkLocalUnicast() && ip.IsGlobalUnicast())
}

type didlResource struct {
	uri      string
	duration float64
}

func metadata(raw, currentURI string) playback.Track {
	var result playback.Track
	var resources []didlResource
	decoder := xml.NewDecoder(strings.NewReader(raw))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "title", "artist", "creator", "album":
			var text string
			decoder.DecodeElement(&text, &start)
			switch start.Name.Local {
			case "title":
				result.Title = text
			case "artist":
				result.Artist = text
			case "creator":
				if result.Artist == "" {
					result.Artist = text
				}
			case "album":
				result.Album = text
			}
		case "res":
			duration := 0.0
			for _, a := range start.Attr {
				if a.Name.Local == "duration" {
					duration, _ = parseTime(a.Value)
				}
			}
			var text string
			decoder.DecodeElement(&text, &start)
			resources = append(resources, didlResource{uri: strings.TrimSpace(text), duration: duration})
		}
	}
	// A DIDL item may list several resources. Duration belongs to the
	// resource that the controller actually selected, not the last one.
	for _, resource := range resources {
		if resource.uri != "" && resource.uri == currentURI {
			result.Duration = resource.duration
			return result
		}
	}
	for _, resource := range resources {
		if resource.duration > 0 {
			result.Duration = resource.duration
			break
		}
	}
	return result
}
func parseTime(raw string) (float64, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0, errors.New("Invalid time")
	}
	h, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	sec, e3 := strconv.ParseFloat(parts[2], 64)
	if e1 != nil || e2 != nil || e3 != nil || h < 0 || h > 24 || m < 0 || m > 59 || sec < 0 || sec >= 60 {
		return 0, errors.New("Invalid time")
	}
	return float64(h*3600+m*60) + sec, nil
}
func formatTime(seconds float64) string {
	n := max(0, int(seconds))
	return fmt.Sprintf("%02d:%02d:%02d", n/3600, n/60%60, n%60)
}

func metaPresence(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "empty"
	}
	return "present"
}

// Playback completion drives the queue independently of UPnP subscriptions.
// Promotion happens in the protocol snapshot; the engine work is queued.
func (s *Server) lifecycle() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.engine.Changes():
			if s.ctx.Err() == nil {
				s.advanceLocked(s.engine.Status())
			}
		}
	}
}
