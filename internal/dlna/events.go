package dlna

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request, service string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, sub := range s.subscriptions {
		if now.After(sub.expires) {
			delete(s.subscriptions, id)
		}
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		w.WriteHeader(412)
		return
	}
	id := r.Header.Get("SID")
	seconds := 1800
	if r.Method == "SUBSCRIBE" {
		timeout := r.Header.Get("TIMEOUT")
		if timeout != "" && !strings.EqualFold(timeout, "Second-infinite") {
			if len(timeout) > 32 || !strings.HasPrefix(strings.ToLower(timeout), "second-") {
				w.WriteHeader(400)
				return
			}
			value, err := strconv.Atoi(timeout[7:])
			if err != nil || value <= 0 {
				w.WriteHeader(400)
				return
			}
			seconds = min(1800, value)
		}
	}

	if r.Method == "UNSUBSCRIBE" {
		sub := s.subscriptions[id]
		if sub == nil || sub.peer != peer || sub.service != service {
			w.WriteHeader(412)
			return
		}
		delete(s.subscriptions, id)
		w.WriteHeader(200)
		return
	}
	if id != "" {
		sub := s.subscriptions[id]
		if sub == nil || sub.peer != peer || sub.service != service || (r.Header.Get("CALLBACK") != "" || r.Header.Get("NT") != "") {
			w.WriteHeader(412)
			return
		}
		sub.expires = now.Add(time.Duration(seconds) * time.Second)
	} else {
		if r.Header.Get("NT") != "upnp:event" || len(s.subscriptions) >= 32 {
			w.WriteHeader(412)
			return
		}
		callback := strings.Trim(r.Header.Get("CALLBACK"), "<>")
		u, err := url.Parse(callback)
		if err != nil || u.Scheme != "http" || u.User != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).Equal(net.ParseIP(peer)) {
			w.WriteHeader(412)
			return
		}
		portText := u.Port()
		if portText == "" {
			portText = "80"
		}
		if port, err := strconv.Atoi(portText); err != nil || port <= 0 || port > 65535 {
			w.WriteHeader(412)
			return
		}
		perPeer := 0
		for _, sub := range s.subscriptions {
			if sub.peer == peer {
				perPeer++
			}
		}
		if perPeer >= 6 {
			w.WriteHeader(503)
			return
		}
		b := make([]byte, 16)
		rand.Read(b)
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		h := hex.EncodeToString(b)
		id = "uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		s.subscriptions[id] = &subscription{id: id, service: service, callback: callback, peer: peer, expires: now.Add(time.Duration(seconds) * time.Second)}
	}
	w.Header().Set("SID", id)
	w.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(seconds))
	w.WriteHeader(200)
}
func (s *Server) eventBody(service string) string {
	status := s.engine.Status()
	state := "STOPPED"
	if status.SessionID == s.owner && status.Protocol == "dlna" {
		switch status.State {
		case "playing":
			state = "PLAYING"
		case "paused":
			state = "PAUSED_PLAYBACK"
		case "transitioning", "waiting_for_speakers":
			state = "TRANSITIONING"
		}
	}
	property := ""
	switch service {
	case "AVTransport":
		values := []struct{ name, value string }{{"TransportState", state}, {"TransportStatus", "OK"}, {"TransportPlaySpeed", "1"}, {"AVTransportURI", s.uri}, {"AVTransportURIMetaData", s.metadata}, {"CurrentTrackURI", s.uri}, {"CurrentTrackMetaData", s.metadata}, {"CurrentTrackDuration", formatTime(s.track.Duration)}, {"CurrentMediaDuration", formatTime(s.track.Duration)}, {"NextAVTransportURI", s.nextURI}, {"NextAVTransportURIMetaData", s.nextMetadata}}
		if status.State == "error" && status.SessionID == s.owner {
			values[1].value = "ERROR_OCCURRED"
		}
		var b strings.Builder
		b.WriteString(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">`)
		for _, v := range values {
			fmt.Fprintf(&b, `<%s val="%s"/>`, v.name, escape(v.value))
		}
		b.WriteString(`</InstanceID></Event>`)
		last := b.String()

		property = "<LastChange>" + escape(last) + "</LastChange>"
	case "RenderingControl":
		mute := "0"
		if s.muted {
			mute = "1"
		}
		last := fmt.Sprintf(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/RCS/"><InstanceID val="0"><Volume channel="Master" val="%d"/><Mute channel="Master" val="%s"/></InstanceID></Event>`, s.volume, mute)
		property = "<LastChange>" + escape(last) + "</LastChange>"
	default:
		property = "<SourceProtocolInfo></SourceProtocolInfo><SinkProtocolInfo>" + escape(protocols) + "</SinkProtocolInfo><CurrentConnectionIDs>0</CurrentConnectionIDs>"
	}
	return `<?xml version="1.0"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property>` + property + "</e:property></e:propertyset>"
}
func (s *Server) events() {
	defer s.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	gate := make(chan struct{}, 4)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.advanceLocked(s.engine.Status())
			for id, sub := range s.subscriptions {
				if time.Now().After(sub.expires) {
					delete(s.subscriptions, id)
					continue
				}
				if sub.busy {
					continue
				}
				body := s.eventBody(sub.service)
				if body == sub.last {
					continue
				}
				select {
				case gate <- struct{}{}:
				default:
					continue
				}
				sub.busy = true
				copySub := *sub
				s.wg.Add(1)
				go func(sub subscription, body string) {
					defer s.wg.Done()
					defer func() { <-gate }()
					req, err := http.NewRequestWithContext(s.ctx, "NOTIFY", sub.callback, bytes.NewBufferString(body))
					if err == nil {
						req.Header.Set("CONTENT-TYPE", "text/xml; charset=utf-8")
						req.Header.Set("NT", "upnp:event")
						req.Header.Set("NTS", "upnp:propchange")
						req.Header.Set("SID", sub.id)
						req.Header.Set("SEQ", strconv.FormatUint(uint64(sub.sequence), 10))
						resp, e := client.Do(req)
						err = e
						if resp != nil {
							io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
							resp.Body.Close()
							if resp.StatusCode < 200 || resp.StatusCode >= 300 {
								err = fmt.Errorf("callback rejected")
							}
						}
					}
					s.mu.Lock()
					if current := s.subscriptions[sub.id]; current != nil {
						current.busy = false
						if err == nil {
							current.last = body
							current.sequence++
						}
					}
					s.mu.Unlock()
				}(copySub, body)
			}
			s.mu.Unlock()
		}
	}
}
