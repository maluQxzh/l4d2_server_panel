package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

func (e *Executor) waitForStartHealth(ctx context.Context, record InstanceRecord, payload operationPayload, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hosts := localGameEndpointHosts(payload.RCONHost, record.RCONHost)
	port := gameEndpointPort(record, payload)
	if port <= 0 || port > 65535 {
		return errors.New("invalid game port for health checks")
	}
	var lastA2SHost string
	var lastInfoErr, lastRCONError error
	for {
		state := e.processes.State(record.ID)
		if state.Status == "stopped" || state.Status == "crash_loop" {
			return fmt.Errorf("server process exited during health check: %s", state.ExitError)
		}
		if state.Status == "degraded" {
			return fmt.Errorf("server process degraded during health check: %s", state.ExitError)
		}
		lastA2SHost, lastInfoErr, lastRCONError = checkStartHealthOnLocalHosts(checkCtx, hosts, port, payload.RCONPassword, 2*time.Second)
		if lastInfoErr == nil && lastRCONError == nil {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-checkCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if lastInfoErr == nil {
				return fmt.Errorf("timed out waiting for RCON on A2S address %s: %v", lastA2SHost, lastRCONError)
			}
			return fmt.Errorf("timed out waiting for A2S (%v); RCON was not checked without a responding local game address", lastInfoErr)
		case <-timer.C:
		}
	}
}

func checkStartHealthOnLocalHosts(ctx context.Context, hosts []string, port int, password string, timeout time.Duration) (string, error, error) {
	infoCtx, infoCancel := context.WithTimeout(ctx, timeout)
	_, host, infoErr := queryA2SInfoOnLocalHosts(infoCtx, hosts, port, timeout)
	infoCancel()
	if infoErr != nil {
		return "", infoErr, errors.New("RCON not checked because A2S did not respond")
	}
	rconCtx, rconCancel := context.WithTimeout(ctx, timeout)
	_, rconErr := (RCONClient{Timeout: timeout}).Execute(rconCtx, net.JoinHostPort(host, strconv.Itoa(port)), password, "status")
	rconCancel()
	if rconErr != nil {
		rconErr = fmt.Errorf("RCON on A2S address %s: %w", host, rconErr)
	}
	return host, nil, rconErr
}
