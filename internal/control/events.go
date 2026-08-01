package control

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type EventBroker struct {
	store       *Store
	appendEvent func(context.Context, Event) (Event, error)
	// persistentMu orders SQLite commit and in-process broadcast as one operation
	// so browser cursors observe the same order as durable events.
	persistentMu sync.Mutex
	mu           sync.RWMutex
	next         uint64
	subs         map[uint64]chan Event
	// authorizationGeneration lets rare credential/resource mutations wake all
	// live streams without coupling their hot event-delivery path to SQLite.
	// A periodic database revalidation remains the cross-check for expiry and
	// changes made outside this Control process.
	authorizationGeneration atomic.Uint64
}

func NewEventBroker(store *Store) *EventBroker {
	broker := &EventBroker{store: store, subs: make(map[uint64]chan Event)}
	if store != nil {
		broker.appendEvent = store.AppendEvent
	}
	broker.authorizationGeneration.Store(1)
	return broker
}

func (b *EventBroker) AuthorizationGeneration() uint64 {
	return b.authorizationGeneration.Load()
}

func (b *EventBroker) InvalidateAuthorization() uint64 {
	return b.authorizationGeneration.Add(1)
}

func (b *EventBroker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	b.next++
	id := b.next
	ch := make(chan Event, 128)
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
}

func (b *EventBroker) Publish(ctx context.Context, event Event, persistent bool) (Event, error) {
	if persistent {
		// Keep ID allocation/commit and broadcast in the same critical section.
		// This is intentionally process-local because the first release supports
		// exactly one Control instance.
		b.persistentMu.Lock()
		defer b.persistentMu.Unlock()
		appendEvent := b.appendEvent
		if appendEvent == nil && b.store != nil {
			appendEvent = b.store.AppendEvent
		}
		if appendEvent == nil {
			return Event{}, errors.New("persistent event store is unavailable")
		}
		var err error
		event, err = appendEvent(ctx, event)
		if err != nil {
			return Event{}, err
		}
		b.broadcast(event)
		return event, nil
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	b.broadcast(event)
	return event, nil
}

func (b *EventBroker) broadcast(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, subscriber := range b.subs {
		select {
		case subscriber <- event:
		default:
		}
	}
}
