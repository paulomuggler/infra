package handler

import (
	"sync"
	"sync/atomic"
)

// MultiplexedChannel fans out values written to Source to every subscriber
// obtained via Fork. Each subscriber send is guarded by a done channel so
// a cancelled consumer can never wedge the fan-out loop.
type MultiplexedChannel[T any] struct {
	Source chan T

	mu       sync.RWMutex
	channels []*subscriber[T]
	exited   atomic.Bool
}

type subscriber[T any] struct {
	ch   chan T
	done chan struct{}
	once sync.Once
}

// cancel marks the subscriber as gone. Idempotent and non-blocking.
func (s *subscriber[T]) cancel() {
	s.once.Do(func() {
		close(s.done)
	})
}

// isCancelled reports whether cancel has been called.
func (s *subscriber[T]) isCancelled() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func NewMultiplexedChannel[T any](buffer int) *MultiplexedChannel[T] {
	c := &MultiplexedChannel[T]{
		Source: make(chan T, buffer),
	}

	go c.run()

	return c
}

// run is the fan-out loop. It delivers each Source value to every live
// subscriber and closes all consumer channels when Source is closed.
func (c *MultiplexedChannel[T]) run() {
	for v := range c.Source {
		c.mu.RLock()
		subs := c.channels
		c.mu.RUnlock()

		for _, s := range subs {
			// Skip already-cancelled subscribers.
			if s.isCancelled() {
				continue
			}

			select {
			case s.ch <- v:
			case <-s.done:
			}
		}
	}

	c.exited.Store(true)

	// Close all remaining consumer channels so `for range` loops exit.
	c.mu.Lock()
	for _, s := range c.channels {
		s.once.Do(func() {
			close(s.done)
			close(s.ch)
		})
	}
	c.channels = nil
	c.mu.Unlock()
}

// Fork registers a new subscriber and returns its channel plus a cancel func.
// If Source is already closed it returns a pre-closed channel and a no-op cancel.
// The channel is bidirectional for backwards compat with start.go which writes
// a bootstrap event into it; new callers should treat it as receive-only.
func (c *MultiplexedChannel[T]) Fork() (chan T, func()) {
	if c.exited.Load() {
		ch := make(chan T)
		close(ch)

		return ch, func() {}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Re-check under lock in case run() finished between the fast path and here.
	if c.exited.Load() {
		ch := make(chan T)
		close(ch)

		return ch, func() {}
	}

	s := &subscriber[T]{
		ch:   make(chan T),
		done: make(chan struct{}),
	}

	c.channels = append(c.channels, s)

	return s.ch, func() {
		c.remove(s)
	}
}

// remove unsubscribes s. Safe to call multiple times.
func (c *MultiplexedChannel[T]) remove(s *subscriber[T]) {
	// Cancel before locking so an in-flight fan-out send can unblock.
	s.cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	for i, sub := range c.channels {
		if sub == s {
			c.channels = append(c.channels[:i], c.channels[i+1:]...)

			return
		}
	}
}
