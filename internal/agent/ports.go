package agent

import (
	"errors"
	"fmt"
	"strconv"
)

func ProbeLocalPorts(ports ...int) error {
	seen := make(map[int]bool)
	for _, port := range ports {
		if port == 0 {
			continue
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid local port %d", port)
		}
		if seen[port] {
			return errors.New("game and SourceTV ports must be different")
		}
		seen[port] = true
		address := "0.0.0.0:" + strconv.Itoa(port)
		udp, err := listenExclusivePacket(address)
		if err != nil {
			return fmt.Errorf("UDP port %d is already occupied or unavailable: %w", port, err)
		}
		tcp, err := listenExclusiveTCP(address)
		if err != nil {
			udp.Close()
			return fmt.Errorf("TCP port %d is already occupied or unavailable: %w", port, err)
		}
		_ = tcp.Close()
		_ = udp.Close()
	}
	return nil
}
