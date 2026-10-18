package playback

import (
	"context"
	"errors"
	"io"
	"micastgo/internal/version"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Media downloads are allowed from the LAN, but never from loopback or
// link-local service addresses. Resolve and validate again when dialing.
func mediaTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || !ip.IP.IsGlobalUnicast() {
				continue
			}
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, errors.New("媒体地址不可访问")
	}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second}
}
func validMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https")
}
func (e *Engine) PlayMedia(raw string, track Track, position float64, continuation ...bool) (string, error) {
	receipt, err := e.StartFile(StartRequest{Protocol: "dlna", URI: raw, Track: track, Position: position, Continuation: len(continuation) > 0 && continuation[0]})
	return receipt.Identity, err
}

// AdvanceMedia validates completion and replaces the generation in one
// transaction. A delayed completion must never replace a newer AirPlay cast.
func (e *Engine) AdvanceMedia(owner, raw string, track Track) (string, error) {
	receipt, err := e.AdvanceFile(StartRequest{Owner: owner, URI: raw, Track: track, Continuation: true})
	return receipt.Identity, err
}

// AdvanceFile is AdvanceMedia with an explicit command order.
func (e *Engine) AdvanceFile(req StartRequest) (Receipt, error) {
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.RLock()
	s := e.current
	e.mu.RUnlock()
	if s == nil || req.Owner == "" || s.identity != req.Owner || s.mode != fileSource {
		return Receipt{}, errors.New("播放会话已被新的投放替代")
	}
	s.mu.Lock()
	finished := s.finished && s.state == "stopped"
	s.mu.Unlock()
	if !finished {
		return Receipt{}, errors.New("当前媒体尚未播放结束")
	}
	return e.startLocked(startSpec{mode: fileSource, protocol: "dlna", trackID: req.TrackID, owner: req.Owner, uri: req.URI, track: req.Track, position: 0, continuation: true, seq: req.Seq})
}
func (e *Engine) download(s *session, raw string) (string, error) {
	e.cacheMu.Lock()
	if e.cacheURL == raw && e.cachePath != "" {
		path := e.cachePath
		e.cacheMu.Unlock()
		return path, nil
	}
	e.cacheMu.Unlock()
	path := ""
	if p := s.prefetched; p != nil {
		select {
		case <-s.ctx.Done():
			p.cancel()
			return "", s.ctx.Err()
		case <-p.done:
		}
		p.mu.Lock()
		path = p.path
		p.path = ""
		p.mu.Unlock()
		p.cancel()
	}
	var err error
	if path == "" {
		path, err = e.fetchMedia(s.ctx, raw)
		if err != nil {
			return "", err
		}
	}
	e.op.Lock()
	defer e.op.Unlock()
	if !e.isCurrent(s) || s.ctx.Err() != nil {
		os.Remove(path)
		return "", context.Canceled
	}
	e.cacheMu.Lock()
	old := e.cachePath
	e.cachePath, e.cacheURL = path, raw
	e.cacheMu.Unlock()
	if old != "" && old != path {
		os.Remove(old)
	}
	return path, nil
}
func (e *Engine) fetchMedia(parent context.Context, raw string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	transport := mediaTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 120 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 6 || !validMediaURL(req.URL.String()) {
			return errors.New("无效媒体重定向")
		}
		return nil
	}}
	if e.transport != nil {
		client.Transport = e.transport
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "MiCast-Go/"+version.Version)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	const maximum = 200 * 1024 * 1024
	if resp.StatusCode != 200 || resp.ContentLength > maximum {
		return "", errors.New("媒体响应失败或超过 200 MiB")
	}
	f, err := os.CreateTemp(e.cacheDir, "media-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(path)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maximum+1))
	if err != nil || n == 0 || n > maximum {
		return "", errors.New("媒体下载未完成或超过 200 MiB")
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	keep = true
	return path, nil
}
