package agent

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestPackageDownloadIdleTimeoutConnStopsStalledRead(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	connection := &packageDownloadIdleTimeoutConn{Conn: client, idleTimeout: 50 * time.Millisecond}
	started := time.Now()
	_, err := connection.Read(make([]byte, 1))
	if !errors.Is(err, errPackageDownloadStalled) {
		t.Fatalf("expected stalled download error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled read took too long to stop: %s", elapsed)
	}
}

func TestPackageDownloadIdleTimeoutConnAllowsContinuingProgress(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	connection := &packageDownloadIdleTimeoutConn{Conn: client, idleTimeout: 250 * time.Millisecond}
	go func() {
		for _, value := range []byte("abc") {
			time.Sleep(25 * time.Millisecond)
			_, _ = server.Write([]byte{value})
		}
	}()

	buffer := make([]byte, 3)
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("read continuing download progress: %v", err)
	}
	if string(buffer) != "abc" {
		t.Fatalf("unexpected download body %q", buffer)
	}
}
