package netutil

import (
	"net"
	"testing"
)

func TestOccupiedPortFallsBackWithoutTakingExistingService(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	listener, err := ListenTCP("tcp4", occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if listener.Addr().String() == occupied.Addr().String() {
		t.Fatal("reused occupied port")
	}
	for _, address := range []string{occupied.Addr().String(), listener.Addr().String()} {
		c, err := net.Dial("tcp4", address)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
}
