package netutil

import (
	"errors"
	"log/slog"
	"net"
	"runtime"
	"syscall"
)

// ListenTCP retains a preferred port when available, and asks the OS for an
// available port when another application already owns it. The bound listener
// is retained; there is no probe-and-rebind race.
func ListenTCP(network, address string) (net.Listener, error) {
	listener, err := net.Listen(network, address)
	if err == nil || !(errors.Is(err, syscall.EADDRINUSE) || (runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10048)))) {
		return listener, err
	}
	host, port, splitErr := net.SplitHostPort(address)
	if splitErr != nil || port == "0" {
		return nil, err
	}
	listener, err = net.Listen(network, net.JoinHostPort(host, "0"))
	if err == nil {
		slog.Info("首选端口已被占用，自动使用空闲端口", "preferred", address, "actual", listener.Addr().String())
	}
	return listener, err
}
