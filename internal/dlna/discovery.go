package dlna

import (
	"context"
	"fmt"
	"micastgo/internal/version"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// Discovery matches MiCast's single SSDP listener with one virtual renderer
// per active target. Receiver changes do not rebind the fixed discovery port.
type Discovery struct {
	conn    *net.UDPConn
	cancel  context.CancelFunc
	ctx     context.Context
	mu      sync.Mutex
	servers map[*Server]bool
	wg      sync.WaitGroup
}

func NewDiscovery(ctx context.Context, iface *net.Interface) (*Discovery, error) {
	conn, err := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900})
	if err != nil {
		return nil, err
	}
	packet := ipv4.NewPacketConn(conn)
	packet.SetMulticastTTL(2)
	packet.SetMulticastLoopback(true)
	if iface != nil {
		packet.SetMulticastInterface(iface)
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &Discovery{conn: conn, ctx: ctx, cancel: cancel, servers: map[*Server]bool{}}
	d.wg.Add(2)
	go d.receive()
	go d.announce()
	return d, nil
}
func (d *Discovery) Add(s *Server) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s.discovery = d
	d.servers[s] = true
	s.notifyOn(d.conn, "ssdp:alive")
}
func (d *Discovery) Remove(s *Server) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.servers[s] {
		s.notifyOn(d.conn, "ssdp:byebye")
		delete(d.servers, s)
	}
}
func (d *Discovery) announce() {
	defer d.wg.Done()
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			for s := range d.servers {
				s.notifyOn(d.conn, "ssdp:alive")
			}
			d.mu.Unlock()
		}
	}
}
func (d *Discovery) receive() {
	defer d.wg.Done()
	buf := make([]byte, 8192)
	for {
		n, peer, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		d.respond(string(buf[:n]), peer)
	}
}
func (d *Discovery) respond(message string, peer *net.UDPAddr) {
	if !strings.HasPrefix(message, "M-SEARCH ") || !strings.Contains(strings.ToLower(message), "ssdp:discover") {
		return
	}
	st := "ssdp:all"
	for _, line := range strings.Split(message, "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "ST") {
			st = strings.TrimSpace(value)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for s := range d.servers {
		for _, target := range s.types() {
			if st != "ssdp:all" && st != target {
				continue
			}
			response := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nEXT:\r\nLOCATION: %s/dlna/description.xml\r\nSERVER: MiCast-Go/%s UPnP/1.0 DLNA/1.5\r\nST: %s\r\nUSN: %s\r\n\r\n", s.base, version.Version, target, s.usn(target))
			response = strings.TrimSuffix(response, "\r\n\r\n") + fmt.Sprintf("\r\nDATE: %s\r\nBOOTID.UPNP.ORG: %d\r\nCONFIGID.UPNP.ORG: 1\r\n\r\n", time.Now().UTC().Format(http.TimeFormat), s.bootID)
			d.conn.WriteToUDP([]byte(response), peer)
		}
	}
}
func (d *Discovery) Close() { d.cancel(); d.conn.Close(); d.wg.Wait() }
