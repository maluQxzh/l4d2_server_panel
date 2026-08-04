package agent

import (
	"context"
	"fmt"
	"time"
)

func (e *Executor) waitForStartHealth(ctx context.Context, record InstanceRecord, payload operationPayload, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastInfoErr, lastRCONError error
	for {
		state := e.processes.State(record.ID)
		if state.Status == "stopped" || state.Status == "crash_loop" {
			return fmt.Errorf("server process exited during health check: %s", state.ExitError)
		}
		infoCtx, infoCancel := context.WithTimeout(checkCtx, 2*time.Second)
		_, lastInfoErr = queryLocalA2SInfo(infoCtx, record, payload, 2*time.Second)
		infoCancel()
		rconCtx, rconCancel := context.WithTimeout(checkCtx, 2*time.Second)
		_, lastRCONError = e.executeInstanceRCON(rconCtx, record, payload, payload.RCONPassword, "status", 2*time.Second)
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
