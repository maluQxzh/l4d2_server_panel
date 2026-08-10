package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestStartHealthUsesRCONOnRespondingA2SAddress(t *testing.T) {
	t.Parallel()
	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	port := tcpListener.Addr().(*net.TCPAddr).Port
	udpListener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udpListener.Close()

	serverErrors := make(chan error, 2)
	go serveTestA2SInfo(udpListener, serverErrors)
	go serveTestRCON(tcpListener, serverErrors)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, infoErr, rconErr := checkStartHealthOnLocalHosts(
		ctx,
		[]string{"192.0.2.1", "127.0.0.1"},
		port,
		"secret",
		time.Second,
	)
	if infoErr != nil || rconErr != nil {
		t.Fatalf("health check failed: A2S=%v RCON=%v", infoErr, rconErr)
	}
	if host != "127.0.0.1" {
		t.Fatalf("responding A2S host = %q, want 127.0.0.1", host)
	}
	for range 2 {
		if err := <-serverErrors; err != nil {
			t.Fatalf("mock game server: %v", err)
		}
	}
}

func TestExecuteRCONOnA2SHostSkipsUnresponsiveAddress(t *testing.T) {
	t.Parallel()
	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	port := tcpListener.Addr().(*net.TCPAddr).Port
	udpListener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udpListener.Close()

	serverErrors := make(chan error, 2)
	go serveTestA2SInfo(udpListener, serverErrors)
	go serveTestRCON(tcpListener, serverErrors)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := executeRCONOnA2SHost(
		ctx,
		[]string{"192.0.2.1", "127.0.0.1"},
		port,
		"secret",
		"meta version",
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("execute RCON on responding A2S address: %v", err)
	}
	if response != "hostname: test" {
		t.Fatalf("response = %q", response)
	}
	for range 2 {
		if err := <-serverErrors; err != nil {
			t.Fatalf("mock game server: %v", err)
		}
	}
}

func serveTestA2SInfo(listener *net.UDPConn, result chan<- error) {
	request := make([]byte, 1024)
	_ = listener.SetDeadline(time.Now().Add(3 * time.Second))
	_, address, err := listener.ReadFromUDP(request)
	if err == nil {
		_, err = listener.WriteToUDP(testA2SInfoResponse(), address)
	}
	result <- err
}

func testA2SInfoResponse() []byte {
	response := bytes.NewBuffer(nil)
	response.Write(a2sHeader)
	response.WriteByte(0x49)
	response.WriteByte(17)
	for _, value := range []string{"L4D2 test", "c1m1_hotel", "left4dead2", "Left 4 Dead 2"} {
		response.WriteString(value)
		response.WriteByte(0)
	}
	_ = binary.Write(response, binary.LittleEndian, uint16(550))
	response.Write([]byte{0, 8, 0, 'd', 'w', 0, 0})
	response.WriteString("2.2.5.1")
	response.WriteByte(0)
	return response.Bytes()
}

func serveTestRCON(listener net.Listener, result chan<- error) {
	conn, err := listener.Accept()
	if err != nil {
		result <- err
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
	result <- err
}
