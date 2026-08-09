package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestStopMarksIntentBeforeGracefulRCONQuit(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	manager, err := NewProcessManager(dataDir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.items["instance"] = &managedProcess{
		spec:  ProcessSpec{InstanceID: "instance", AutoRestart: true},
		cmd:   &exec.Cmd{},
		state: ProcessState{Status: "running"},
		done:  make(chan struct{}),
	}
	executor.processes = manager

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	record, _ := executor.registry.Get("instance")
	record.AutoRestart = true
	record.DesiredRunning = true
	record.DesiredStateKnown = true
	record.RCONHost = "127.0.0.1"
	record.RCONPort = port
	if err := executor.registry.Put(record); err != nil {
		t.Fatal(err)
	}

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
		if err == nil && command.Body != "quit" {
			err = errors.New("expected RCON quit command")
		}

		manager.mu.Lock()
		if err == nil && !manager.items["instance"].stopRequested {
			err = errors.New("stop intent was not recorded before RCON quit")
		}
		manager.items["instance"].cmd = nil
		manager.items["instance"].state = ProcessState{Status: "stopped"}
		manager.mu.Unlock()

		if err == nil {
			err = writeRCONPacket(conn, rconPacket{ID: command.ID, Type: rconResponseValue})
		}
		if err == nil {
			err = writeRCONPacket(conn, rconPacket{ID: sentinel.ID, Type: rconResponseValue})
		}
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := executor.stop(ctx, domain.Job{
		InstanceID: "instance",
		Type:       "stop",
		Payload:    map[string]any{"rcon_password": "secret", "timeout_seconds": 2},
	}, func(int, string) {})
	if err != nil {
		t.Fatalf("stop instance: %v", err)
	}
	if result["status"] != "stopped" || result["graceful"] != true {
		t.Fatalf("stop result = %#v", result)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("mock RCON server: %v", err)
	}
	record, _ = executor.registry.Get("instance")
	if record.DesiredRunning {
		t.Fatal("explicit stop did not persist desired stopped state")
	}
}

func TestRequestStopCancelsPendingAutomaticRestart(t *testing.T) {
	p := &managedProcess{
		spec:  ProcessSpec{InstanceID: "instance", AutoRestart: true},
		state: ProcessState{Status: "stopped"},
		done:  make(chan struct{}),
	}
	manager := &ProcessManager{
		items:  map[string]*managedProcess{"instance": p},
		logger: log.New(io.Discard, "", 0),
	}

	manager.RequestStop("instance")
	manager.restartAfterCrash(p)

	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if !p.stopRequested {
		t.Fatal("stop intent was not retained")
	}
	if p.launching || p.state.Status != "stopped" {
		t.Fatalf("pending automatic restart was not canceled: state=%+v launching=%v", p.state, p.launching)
	}
}
