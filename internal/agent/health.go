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
	host, port := rconEndpoint(record, payload)
	if port <= 0 || port > 65535 {
		return errors.New("invalid health-check port")
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	var lastInfoErr, lastRCONError error
	for {
		state := e.processes.State(record.ID)
		if state.Status == "stopped" || state.Status == "crash_loop" {
			return fmt.Errorf("server process exited during health check: %s", state.ExitError)
		}
		infoCtx, infoCancel := context.WithTimeout(checkCtx, 2*time.Second)
		_, lastInfoErr = (A2SClient{Timeout: 2 * time.Second}).Info(infoCtx, address)
		infoCancel()
		rconCtx, rconCancel := context.WithTimeout(checkCtx, 2*time.Second)
		_, lastRCONError = (RCONClient{Timeout: 2 * time.Second}).Execute(rconCtx, address, payload.RCONPassword, "status")
		rconCancel()
		if lastInfoErr == nil && lastRCONError == nil {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-checkCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("timed out waiting for A2S (%v) and RCON (%v)", lastInfoErr, lastRCONError)
		case <-timer.C:
		}
	}
}
