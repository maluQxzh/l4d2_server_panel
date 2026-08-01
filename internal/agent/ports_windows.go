//go:build windows

package agent

import (
	"context"
	"net"
	"syscall"
)

const soExclusiveAddrUse = -5

func exclusiveListenConfig() net.ListenConfig {
	return net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
		}); err != nil {
			return err
		}
		return optionErr
	}}
}

func listenExclusivePacket(address string) (net.PacketConn, error) {
	config := exclusiveListenConfig()
	return config.ListenPacket(context.Background(), "udp4", address)
}
func listenExclusiveTCP(address string) (net.Listener, error) {
	config := exclusiveListenConfig()
	return config.Listen(context.Background(), "tcp4", address)
}
