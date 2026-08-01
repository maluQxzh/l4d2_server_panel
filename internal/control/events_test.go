package control

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEphemeralEventHasTimestampAndZeroCursor(t *testing.T) {
	broker := NewEventBroker(nil)
	event, err := broker.Publish(context.Background(), Event{Type: "metric"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != 0 {
		t.Fatalf("ephemeral event received persistent cursor %d", event.ID)
	}
	if event.CreatedAt.IsZero() {
		t.Fatal("ephemeral event did not receive a timestamp")
	}
}

func TestPersistentPublishSerializesCommitBeforeBroadcast(t *testing.T) {
	broker := NewEventBroker(nil)
	live, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	broker.appendEvent = func(_ context.Context, event Event) (Event, error) {
		call := calls.Add(1)
		event.ID = int64(call)
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
		} else if call == 2 {
			close(secondEntered)
		}
		return event, nil
	}
	results := make(chan error, 2)
	go func() {
		_, err := broker.Publish(context.Background(), Event{Type: "first"}, true)
		results <- err
	}()
	<-firstEntered
	go func() {
		close(secondStarted)
		_, err := broker.Publish(context.Background(), Event{Type: "second"}, true)
		results <- err
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		t.Fatal("second persistent append entered before the first commit/broadcast was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second persistent append never ran after the first was released")
	}
	for want := int64(1); want <= 2; want++ {
		if event := <-live; event.ID != want {
			t.Fatalf("persistent broadcast ID=%d, want %d", event.ID, want)
		}
	}
}

func TestPersistentPublishOrderingUnderConcurrency(t *testing.T) {
	broker := NewEventBroker(nil)
	live, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	var next atomic.Int64
	broker.appendEvent = func(_ context.Context, event Event) (Event, error) {
		event.ID = next.Add(1)
		time.Sleep(time.Duration(event.ID%3) * time.Millisecond)
		return event, nil
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsFound := make(chan error, 100)
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := broker.Publish(context.Background(), Event{Type: "concurrent"}, true)
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	for want := int64(1); want <= 100; want++ {
		if event := <-live; event.ID != want {
			t.Fatalf("concurrent persistent broadcast ID=%d, want %d", event.ID, want)
		}
	}
}

func TestEphemeralPublishBypassesBlockedPersistentAppend(t *testing.T) {
	broker := NewEventBroker(nil)
	live, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	appendEntered := make(chan struct{})
	releaseAppend := make(chan struct{})
	broker.appendEvent = func(_ context.Context, event Event) (Event, error) {
		close(appendEntered)
		<-releaseAppend
		event.ID = 1
		return event, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := broker.Publish(context.Background(), Event{Type: "durable"}, true)
		done <- err
	}()
	<-appendEntered
	if _, err := broker.Publish(context.Background(), Event{Type: "metric"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-live:
		if event.Type != "metric" || event.ID != 0 {
			t.Fatalf("unexpected event while durable append was blocked: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("ephemeral publish waited for the persistent serialization lock")
	}
	close(releaseAppend)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
