package agent

import (
	"net"
	"testing"
)

func TestProbeLocalPortsDetectsUDPConflict(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.LocalAddr().(*net.UDPAddr).Port
	if err := ProbeLocalPorts(port); err == nil {
		t.Fatal("expected occupied UDP port to be rejected")
	}
}

func TestProbeLocalPortsAcceptsAvailablePort(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.LocalAddr().(*net.UDPAddr).Port
	listener.Close()
	if err := ProbeLocalPorts(port); err != nil {
		t.Fatalf("available port rejected: %v", err)
	}
}
