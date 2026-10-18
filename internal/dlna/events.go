package dlna

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"

	"strconv"
	"strings"
	"time"
)

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request, service string) {
	// A flushed empty response otherwise becomes chunked and remains open
	// until the handler returns, racing the initial callback on some clients.
	w.Header().Set("Content-Length", "0")
	result := &subscriptionResponse{ResponseWriter: w, code: 200}
	w = result
	defer func() {
		s.recordProtocol(service, r.Method, result.code, nil)
		if result.code == 200 && r.Method == "SUBSCRIBE" {
			// The original starts the initial NOTIFY after the subscription response.
			// Flush SID first so the controller can associate sequence zero correctly.
			_ = http.NewResponseController(w).Flush()
			select {
			case s.eventsWake <- struct{}{}:
			default:
			}
		}
	}()
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
		// A resumed control point may have discarded its local state while
		// asleep. Renewal must replay a snapshot even if nothing changed.
		sub.revision++
		sub.last = ""
		sub.pending = nil
		sub.retryAt = time.Time{}
		sub.failures = 0
	} else {
		if !strings.EqualFold(r.Header.Get("NT"), "upnp:event") || len(s.subscriptions) >= 32 {
			w.WriteHeader(412)
			return
		}
		targets, err := callbackTargets(r.Context(), r.Header.Get("CALLBACK"), peer)
		if err != nil {
			w.WriteHeader(412)
			return
		}
		perPeer := 0
		for _, sub := range s.subscriptions {
			if sub.peer == peer {
				perPeer++
			}
		}
		if perPeer >= 16 {
			w.WriteHeader(503)
			return
		}
		b := make([]byte, 16)
		rand.Read(b)
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		h := hex.EncodeToString(b)
		id = "uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		s.subscriptions[id] = &subscription{id: id, service: service, callback: targets[0].url, targets: targets, peer: peer, expires: now.Add(time.Duration(seconds) * time.Second)}
	}
	w.Header().Set("SID", id)
	w.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(seconds))
	w.WriteHeader(200)
}
func (s *Server) eventBody(service string) string {
	property := ""
	switch service {
	case "AVTransport":
		status := s.engine.Status()
		s.stateMu.Lock()
		snap := s.snapshotLocked(status)
		s.stateMu.Unlock()
		track := "0"
		tracks := "0"
		if snap.HasMedia {
			track, tracks = "1", "1"
		}
		values := []struct{ name, value string }{
			{"TransportState", snap.State},
			{"TransportStatus", snap.Status},
			{"TransportPlaySpeed", "1"},
			{"AVTransportURI", snap.Current.URI},
			{"AVTransportURIMetaData", snap.Current.Metadata},
			{"CurrentTrack", track},
			{"CurrentTrackURI", snap.Current.URI},
			{"CurrentTrackMetaData", snap.Current.Metadata},
			{"CurrentTrackDuration", formatTime(snap.Duration)},
			{"CurrentMediaDuration", formatTime(snap.Duration)},
			{"NextAVTransportURI", snap.Next.URI},
			{"NextAVTransportURIMetaData", snap.Next.Metadata},
			{"NumberOfTracks", tracks},
			{"CurrentTransportActions", snap.Actions},
		}
		var b strings.Builder
		b.WriteString(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">`)
		for _, v := range values {
			fmt.Fprintf(&b, `<%s val="%s"/>`, v.name, escape(v.value))
		}
		b.WriteString(`</InstanceID></Event>`)
		property = "<LastChange>" + escape(b.String()) + "</LastChange>"
	case "RenderingControl":
		volume, muted := s.engine.VolumeState()
		mute := "0"
		if muted {
			mute = "1"
		}
		last := fmt.Sprintf(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/RCS/"><InstanceID val="0"><Volume channel="Master" val="%d"/><Mute channel="Master" val="%s"/></InstanceID></Event>`, volume, mute)
		property = "<LastChange>" + escape(last) + "</LastChange>"
	default:
		return `<?xml version="1.0" encoding="utf-8"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property><SourceProtocolInfo></SourceProtocolInfo></e:property><e:property><SinkProtocolInfo>` + escape(protocols) + `</SinkProtocolInfo></e:property><e:property><CurrentConnectionIDs>0</CurrentConnectionIDs></e:property></e:propertyset>`
	}
	return `<?xml version="1.0" encoding="utf-8"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property>` + property + "</e:property></e:propertyset>"
}
func (s *Server) events() {
	defer s.wg.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	gate := make(chan struct{}, 4)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.eventsWake:
		}
		{
			s.mu.Lock()
			for id, sub := range s.subscriptions {
				if time.Now().After(sub.expires) {
					delete(s.subscriptions, id)
					continue
				}
				if sub.busy || time.Now().Before(sub.retryAt) {
					continue
				}
				body := s.eventBody(sub.service)
				if len(sub.pending) > 0 {
					body = sub.pending[0]
				}
				if body == sub.last {
					if len(sub.pending) > 0 {
						sub.pending = sub.pending[1:]
					}
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
					code := 0
					var err error
					targets := sub.targets
					if len(targets) == 0 {
						targets = []callbackTarget{{url: sub.callback}}
					}
					for _, target := range targets {
						code = 0
						req, requestErr := http.NewRequestWithContext(s.ctx, "NOTIFY", target.url, bytes.NewBufferString(body))
						err = requestErr
						if err != nil {
							continue
						}
						if target.host != "" {
							req.Host = target.host
						}
						req.Header.Set("CONTENT-TYPE", `text/xml; charset="utf-8"`)
						req.Header.Set("NT", "upnp:event")
						req.Header.Set("NTS", "upnp:propchange")
						req.Header.Set("SID", sub.id)
						req.Header.Set("SEQ", strconv.FormatUint(uint64(sub.sequence), 10))
						sender := client
						var secure *http.Transport
						if req.URL.Scheme == "https" {
							secure = transport.Clone()
							secure.TLSClientConfig = &tls.Config{ServerName: target.tlsName, MinVersion: tls.VersionTLS12}
							sender = &http.Client{Transport: secure, Timeout: client.Timeout, CheckRedirect: client.CheckRedirect}
						}
						resp, sendErr := sender.Do(req)
						err = sendErr
						if resp != nil {
							code = resp.StatusCode
							io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
							resp.Body.Close()
							if code < 200 || code >= 300 {
								err = fmt.Errorf("callback rejected")
							}
						}
						if secure != nil {
							secure.CloseIdleConnections()
						}
						if err == nil {
							break
						}
					}
					s.mu.Lock()
					// Extract the state from the actual notification body, not
					// the newer engine snapshot present when its reply arrives.
					notifiedState := ""
					for _, candidate := range []string{"PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING", "STOPPED"} {
						if strings.Contains(body, `TransportState val="`+candidate+`"`) || strings.Contains(body, `TransportState val=&#34;`+candidate+`&#34;`) {
							notifiedState = candidate
							break
						}
					}
					seq := sub.sequence
					s.appendProtocol(ProtocolEvent{At: time.Now(), Service: sub.service, Action: "NOTIFY", Code: code, State: notifiedState, Sequence: &seq})
					if current := s.subscriptions[sub.id]; current != nil {
						current.busy = false
						if err == nil {
							if current.revision == sub.revision {
								current.last = body
								if len(current.pending) > 0 && current.pending[0] == body {
									current.pending = current.pending[1:]
								}
								current.failures = 0
								current.retryAt = time.Time{}
							}
							if current.sequence == ^uint32(0) {
								current.sequence = 1
							} else {
								current.sequence++
							}
						} else if current.revision == sub.revision {
							current.failures++
							current.retryAt = time.Now().Add(time.Duration(1<<min(current.failures-1, 5)) * time.Second)
						}
					}
					s.mu.Unlock()
				}(copySub, body)
			}
			s.mu.Unlock()
		}
	}
}
