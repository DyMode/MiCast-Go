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
	"os/exec"
	"strconv"
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
	if !validMediaURL(raw) || position < 0 || position > 86400 {
		return "", errors.New("无效媒体地址或播放位置")
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return "", errors.New("投送入口已关闭")
	}
	s := e.newSession("dlna", track, position)
	s.continuation = len(continuation) > 0 && continuation[0]
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		path, err := e.download(s, raw)
		if err != nil {
			if s.ctx.Err() == nil {
				e.fail(s, "媒体下载失败，请检查来源地址和文件大小")
			}
			return
		}
		reader, writer := io.Pipe()
		s.mu.Lock()
		s.source = reader
		s.mu.Unlock()
		defer reader.Close()
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-protocol_whitelist", "file,pipe", "-format_whitelist", "mp3,aac,flac,wav,mov,mp4,m4a,3gp,3g2,mj2,ogg"}
		if position > 0 {
			args = append(args, "-ss", strconv.FormatFloat(position, 'f', 3, 64))
		}
		args = append(args, "-i", path)
		args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-ar", "44100", "-ac", "2", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
		command := exec.CommandContext(s.ctx, e.binary, args...)
		command.Stdout = writer
		command.Stderr = io.Discard
		command.WaitDelay = 2 * time.Second
		if err = command.Start(); err != nil {
			writer.CloseWithError(err)
			e.fail(s, "无法启动媒体解码")
			return
		}
		e.prepare(s)
		done := make(chan struct{})
		go func() { defer close(done); e.consume(s, reader) }()
		err = command.Wait()
		if err != nil && s.ctx.Err() == nil {
			e.fail(s, "媒体解码失败或格式不支持")
		}
		writer.Close()
		<-done
	}()
	return s.id, nil
}
func (e *Engine) download(s *session, raw string) (string, error) {
	e.cacheMu.Lock()
	if e.cacheURL == raw && e.cachePath != "" {
		path := e.cachePath
		e.cacheMu.Unlock()
		return path, nil
	}
	e.cacheMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 120*time.Second)
	defer cancel()
	transport := mediaTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 120 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 6 || !validMediaURL(req.URL.String()) {
			return errors.New("无效媒体重定向")
		}
		return nil
	}}
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
	e.op.Lock()
	defer e.op.Unlock()
	if !e.isCurrent(s) || s.ctx.Err() != nil {
		return "", context.Canceled
	}
	e.cacheMu.Lock()
	old := e.cachePath
	e.cachePath = path
	e.cacheURL = raw
	e.cacheMu.Unlock()
	if old != "" && old != path {
		os.Remove(old)
	}
	keep = true
	return path, nil
}
