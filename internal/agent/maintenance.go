package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (e *Executor) maintenanceCountdown(ctx context.Context, record InstanceRecord, payload operationPayload, progress ProgressFunc) error {
	warnings := payload.WarningSeconds
	if len(warnings) == 0 {
		warnings = []int{600, 300, 60}
	}
	host, port := rconEndpoint(record, payload)
	if port <= 0 || port > 65535 {
		return errors.New("invalid RCON port for maintenance countdown")
	}
	message := strings.TrimSpace(payload.BroadcastMessage)
	if message == "" {
		message = "服务器将在 {seconds} 秒后维护，请做好准备"
	}
	if len(message) > 256 || strings.ContainsAny(message, "\x00\r\n;") {
		return errors.New("broadcast_message contains forbidden characters")
	}
	return runCountdown(ctx, warnings, func(seconds int) error {
		text := message
		if strings.Contains(text, "{seconds}") {
			text = strings.ReplaceAll(text, "{seconds}", strconv.Itoa(seconds))
		} else {
			text = fmt.Sprintf("%s (%d 秒)", text, seconds)
		}
		command := `say "` + strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), `"`, `\"`) + `"`
		_, err := (RCONClient{Timeout: 8 * time.Second}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), payload.RCONPassword, command)
		if err == nil && progress != nil {
			progress(10, fmt.Sprintf("maintenance warning sent: %d seconds", seconds))
		}
		return err
	})
}

func runCountdown(ctx context.Context, warnings []int, broadcast func(int) error) error {
	if broadcast == nil {
		return errors.New("countdown broadcaster is required")
	}
	unique := make(map[int]bool, len(warnings))
	normalized := make([]int, 0, len(warnings))
	for _, seconds := range warnings {
		if seconds <= 0 || seconds > 3600 {
			return errors.New("warning_seconds values must be between 1 and 3600")
		}
		if !unique[seconds] {
			unique[seconds] = true
			normalized = append(normalized, seconds)
		}
	}
	if len(normalized) == 0 {
		return errors.New("at least one warning second is required")
	}
	sort.Sort(sort.Reverse(sort.IntSlice(normalized)))
	for i, seconds := range normalized {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := broadcast(seconds); err != nil {
			return fmt.Errorf("broadcast %d-second maintenance warning: %w", seconds, err)
		}
		next := 0
		if i+1 < len(normalized) {
			next = normalized[i+1]
		}
		delay := time.Duration(seconds-next) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
