package agent

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestRunCountdownShortSchedule(t *testing.T) {
	started := time.Now()
	var sent []int
	err := runCountdown(context.Background(), []int{1, 1}, func(seconds int) error { sent = append(sent, seconds); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, []int{1}) {
		t.Fatalf("warnings = %#v", sent)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("countdown elapsed %s", elapsed)
	}
}

func TestRunCountdownCanBeCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := runCountdown(ctx, []int{2}, func(int) error { cancel(); return nil })
	if err == nil {
		t.Fatal("expected cancellation")
	}
}
