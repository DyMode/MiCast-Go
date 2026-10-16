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
	id, service, callback, peer string
	expires                     time.Time
	sequence                    uint32
	last                        string
	busy                        bool
}
type Server struct {
	bootID                uint32
	discovery             *Discovery
	ownsDiscovery         bool
	nextURI, nextMetadata string
	ctx                   context.Context
	cancel                context.CancelFunc
	engine                *playback.Engine
	name, base, uuid      string
	mu                    sync.Mutex
	uri, metadata, owner  string
	track                 playback.Track
	volume, beforeMute    int
	muted                 bool
	subscriptions         map[string]*subscription
	wg                    sync.WaitGroup
}

func New(ctx context.Context, e *playback.Engine, name, base, identity string) *Server {
	ctx, cancel := context.WithCancel(ctx)
	digest := sha256.Sum256([]byte("micast-go/dlna/" + identity))
	h := hex.EncodeToString(digest[:16])
	uuid := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	s := &Server{bootID: uint32(time.Now().Unix()) + bootSequence.Add(1), ctx: ctx, cancel: cancel, engine: e, name: name, base: strings.TrimRight(base, "/"), uuid: "uuid:" + uuid, volume: 50, subscriptions: map[string]*subscription{}}
	s.wg.Add(1)
	go s.events()
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
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		io.WriteString(w, scpd(service))
	case route == "control" && r.Method == "POST":
		s.control(w, r, service)
	case route == "event" && (r.Method == "SUBSCRIBE" || r.Method == "UNSUBSCRIBE"):
		s.subscribe(w, r, service)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) description(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	baseURL, _ := url.Parse(s.base)
	prefix := strings.TrimRight(baseURL.Path, "/")
	var services strings.Builder
	for _, name := range []string{"AVTransport", "RenderingControl", "ConnectionManager"} {
		fmt.Fprintf(&services, "<service><serviceType>%s</serviceType><serviceId>urn:upnp-org:serviceId:%s</serviceId><SCPDURL>%s/dlna/%s/scpd.xml</SCPDURL><controlURL>%s/dlna/%s/control</controlURL><eventSubURL>%s/dlna/%s/event</eventSubURL></service>", serviceTypes[name], name, escape(prefix), name, escape(prefix), name, escape(prefix), name)
	}
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><URLBase>%s/</URLBase><device><deviceType>%s</deviceType><friendlyName>%s</friendlyName><manufacturer>DyMode</manufacturer><modelName>MiCast Go</modelName><modelNumber>%s</modelNumber><dlna:X_DLNADOC>DMR-1.50</dlna:X_DLNADOC><UDN>%s</UDN><serviceList>%s</serviceList></device></root>`, escape(baseURL.Scheme+"://"+baseURL.Host), renderer, escape(s.name), version.Version, s.uuid, services.String())
}
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
func fault(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(500)
	fmt.Fprintf(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, escape(text))
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
	if err != nil {
		fault(w, 701, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%sResponse xmlns:u="%s">`, action, serviceTypes[service])
	for _, argument := range actions[service][action] {
		parts := strings.Split(argument, ":")
		if parts[1] != "out" {
			continue
		}
		key := parts[0]
		fmt.Fprintf(w, "<%s>%s</%s>", key, escape(response[key]), key)
	}
	fmt.Fprintf(w, "</u:%sResponse></s:Body></s:Envelope>", action)
}
func (s *Server) action(ctx context.Context, service, action string, args map[string]string) (map[string]string, error) {
	response := map[string]string{}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.engine.Status()
	ours := s.owner != "" && status.SessionID == s.owner && status.Protocol == "dlna"
	state := "STOPPED"
	if ours {
		switch status.State {
		case "playing":
			state = "PLAYING"
		case "paused":
			state = "PAUSED_PLAYBACK"
		case "transitioning", "waiting_for_speakers":
			state = "TRANSITIONING"
		}
	}
	switch action {
	case "SetAVTransportURI":
		raw := strings.TrimSpace(args["CurrentURI"])
		if raw == "" || !validURI(raw) {
			return nil, errors.New("Invalid media URI")
		}
		if ours {
			s.engine.Stop(s.owner)
		}
		s.uri = raw
		s.metadata = args["CurrentURIMetaData"]
		s.track = metadata(s.metadata)
		s.owner = ""
		s.nextURI, s.nextMetadata = "", ""
	case "SetNextAVTransportURI":
		raw := strings.TrimSpace(args["NextURI"])
		if raw != "" && !validURI(raw) {
			return nil, errors.New("Invalid media URI")
		}
		s.nextURI = raw
		s.nextMetadata = args["NextURIMetaData"]
	case "Next":
		if err := s.nextLocked(); err != nil {
			return nil, err
		}
	case "Play":
		if s.nextURI != "" && (args["_advance_next"] == "1" || (ours && status.Finished)) {
			if err := s.nextLocked(); err != nil {
				return nil, err
			}
			break
		}
		if s.uri == "" {
			return nil, errors.New("No media")
		}
		if args["Speed"] != "" && args["Speed"] != "1" {
			return nil, errors.New("Unsupported speed")
		}
		if state == "PAUSED_PLAYBACK" {
			if err := s.engine.Resume(ctx); err != nil {
				return nil, err
			}
			s.owner = s.engine.Status().SessionID
		} else if state != "PLAYING" {
			id, err := s.engine.PlayMedia(s.uri, s.track, 0)
			if err != nil {
				return nil, err
			}
			s.owner = id
		}
	case "Pause":
		if !ours {
			return nil, errors.New("No active media")
		}
		if err := s.engine.Pause(ctx); err != nil {
			return nil, err
		}
	case "Stop":
		if ours {
			s.engine.Stop(s.owner)
		}
		s.owner = ""
	case "Seek":
		if !ours || args["Unit"] != "REL_TIME" {
			return nil, errors.New("Seek not available")
		}
		position, err := parseTime(args["Target"])
		if err != nil {
			return nil, err
		}
		if err = s.engine.Seek(position); err != nil {
			return nil, err
		}
		s.owner = s.engine.Status().SessionID
	case "GetTransportInfo":
		response = map[string]string{"CurrentTransportState": state, "CurrentTransportStatus": "OK", "CurrentSpeed": "1"}
		if ours && status.State == "error" {
			response["CurrentTransportStatus"] = "ERROR_OCCURRED"
		}
	case "GetPositionInfo":
		position := 0.0
		if ours {
			position = status.Position
		}
		response = map[string]string{"Track": "1", "TrackDuration": formatTime(s.track.Duration), "TrackMetaData": s.metadata, "TrackURI": s.uri, "RelTime": formatTime(position), "AbsTime": formatTime(position), "RelCount": "2147483647", "AbsCount": "2147483647"}
	case "GetMediaInfo":
		response = map[string]string{"NrTracks": "1", "MediaDuration": formatTime(s.track.Duration), "CurrentURI": s.uri, "CurrentURIMetaData": s.metadata, "NextURI": s.nextURI, "NextURIMetaData": s.nextMetadata, "PlayMedium": "NETWORK", "RecordMedium": "NOT_IMPLEMENTED", "WriteStatus": "NOT_IMPLEMENTED"}
	case "GetDeviceCapabilities":
		response = map[string]string{"PlayMedia": "NETWORK", "RecMedia": "NOT_IMPLEMENTED", "RecQualityModes": "NOT_IMPLEMENTED"}
	case "GetTransportSettings":
		response = map[string]string{"PlayMode": "NORMAL", "RecQualityMode": "NOT_IMPLEMENTED"}
	case "GetCurrentTransportActions":
		response["Actions"] = "Play,Pause,Stop,Seek"
		if s.nextURI != "" {
			response["Actions"] += ",Next"
		}
	case "GetVolume":
		response["CurrentVolume"] = strconv.Itoa(s.volume)
	case "SetVolume":
		volume, err := strconv.Atoi(args["DesiredVolume"])
		if err != nil || volume < 0 || volume > 100 {
			return nil, errors.New("Invalid volume")
		}
		if !s.muted {
			if err = s.engine.Volume(ctx, volume); err != nil {
				return nil, err
			}
		} else {
			s.beforeMute = volume
		}
		s.volume = volume
	case "GetMute":
		response["CurrentMute"] = "0"
		if s.muted {
			response["CurrentMute"] = "1"
		}
	case "SetMute":
		mute := args["DesiredMute"] == "1" || strings.EqualFold(args["DesiredMute"], "true")
		if mute && !s.muted {
			s.beforeMute = s.volume
			if err := s.engine.Volume(ctx, 0); err != nil {
				return nil, err
			}
		} else if !mute && s.muted {
			if err := s.engine.Volume(ctx, s.beforeMute); err != nil {
				return nil, err
			}
			s.volume = s.beforeMute
		}
		s.muted = mute
	case "GetProtocolInfo":
		response = map[string]string{"Source": "", "Sink": protocols}
	case "GetCurrentConnectionIDs":
		response["ConnectionIDs"] = "0"
	case "GetCurrentConnectionInfo":
		response = map[string]string{"RcsID": "0", "AVTransportID": "0", "ProtocolInfo": "", "PeerConnectionManager": "", "PeerConnectionID": "-1", "Direction": "Input", "Status": "OK"}
	default:
		return nil, errors.New("Invalid Action")
	}
	return response, nil
}
func validURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || (!ip.IsLoopback() && !ip.IsLinkLocalUnicast() && ip.IsGlobalUnicast())
}
func metadata(raw string) playback.Track {
	var result playback.Track
	decoder := xml.NewDecoder(strings.NewReader(raw))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if start, ok := token.(xml.StartElement); ok {
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
				for _, a := range start.Attr {
					if a.Name.Local == "duration" {
						result.Duration, _ = parseTime(a.Value)
					}
				}
			}
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

// nextLocked preserves queued metadata until a new session can be created.
func (s *Server) nextLocked() error {
	if s.nextURI == "" {
		return errors.New("No queued media URI")
	}
	track := metadata(s.nextMetadata)
	id, err := s.engine.PlayMedia(s.nextURI, track, 0, true)
	if err != nil {
		return err
	}
	s.uri, s.metadata, s.track, s.owner = s.nextURI, s.nextMetadata, track, id
	s.nextURI, s.nextMetadata = "", ""
	return nil
}
func (s *Server) advanceLocked(status playback.Status) {
	if status.Finished && status.Protocol == "dlna" && status.SessionID == s.owner && s.nextURI != "" {
		_ = s.nextLocked()
	}
}
