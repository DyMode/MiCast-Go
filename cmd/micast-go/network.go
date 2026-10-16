package main

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func advertiseIP(explicit string) (string, *net.Interface, error) {
	if explicit == "" {
		conn, err := net.Dial("udp4", "1.1.1.1:53")
		if err == nil {
			explicit = conn.LocalAddr().(*net.UDPAddr).IP.String()
			conn.Close()
		}
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", nil, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.To4() == nil || !ip.IsGlobalUnicast() {
				continue
			}
			if explicit == "" || ip.String() == explicit {
				return ip.String(), &iface, nil
			}
		}
	}
	return "", nil, errors.New("未找到可用局域网 IPv4 地址，请使用 --advertise-ip 指定")
}
func gatewayHandler(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/app/micast-go"
		if r.URL.Path == prefix {
			http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, prefix+"/") {
			cloned := r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, prefix)
			u.RawPath = ""
			cloned.URL = &u
			base.ServeHTTP(w, cloned)
			return
		}
		base.ServeHTTP(w, r)
	})
}
func unixListener(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("网关路径已被普通文件占用")
		}
		probe, err := net.Dial("unix", path)
		if err == nil {
			probe.Close()
			return nil, errors.New("网关 Socket 已被运行中的应用占用")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0666); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func publicBase(ip string, listener net.Listener) string {
	port := listener.Addr().(*net.TCPAddr).Port
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(port))}).String()
}
