//go:build !windows

package agent

import "net"

func listenExclusivePacket(address string) (net.PacketConn, error) {
	return net.ListenPacket("udp4", address)
}
func listenExclusiveTCP(address string) (net.Listener, error) { return net.Listen("tcp4", address) }
