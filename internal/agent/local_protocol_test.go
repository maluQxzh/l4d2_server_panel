package agent

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestExecuteRCONOnLocalHostsRetriesDialFailure(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		auth, err := readRCONPacket(conn)
		if err == nil {
			err = writeRCONPacket(conn, rconPacket{ID: auth.ID, Type: rconExecCommand})
		}
		var command, sentinel rconPacket
		if err == nil {
			command, err = readRCONPacket(conn)
		}
		if err == nil {
			sentinel, err = readRCONPacket(conn)
		}
		if err == nil {
			err = writeRCONPacket(conn, rconPacket{ID: command.ID, Type: rconResponseValue, Body: "hostname: test"})
		}
		if err == nil {
			err = writeRCONPacket(conn, rconPacket{ID: sentinel.ID, Type: rconResponseValue})
		}
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	response, err := executeRCONOnLocalHosts(ctx, []string{"127.0.0.2", "127.0.0.1"}, port, "secret", "status", time.Second)
	if err != nil {
		t.Fatalf("execute RCON with fallback: %v", err)
	}
	if response != "hostname: test" {
		t.Fatalf("response = %q", response)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("mock RCON server: %v", err)
	}
}
