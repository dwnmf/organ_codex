package inproc

import (
	"errors"
	"sync"

	"organ_codex/internal/domain"
)

var (
	ErrAgentNotRegistered = errors.New("agent is not registered in bus")
	ErrAgentQueueFull     = errors.New("agent queue is full")
)

type Bus struct {
	mu     sync.RWMutex
	subs   map[string]chan domain.Message
	buffer int
}

func New(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 64
	}
	return &Bus{
		subs:   make(map[string]chan domain.Message),
		buffer: buffer,
	}
}

func (b *Bus) Register(agentID string) <-chan domain.Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subs[agentID]; ok {
		return ch
	}
	ch := make(chan domain.Message, b.buffer)
	b.subs[agentID] = ch
	return ch
}

func (b *Bus) Unregister(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch, ok := b.subs[agentID]
	if !ok {
		return
	}
	delete(b.subs, agentID)
	close(ch)
}

func (b *Bus) Publish(msg domain.Message) error {
	b.mu.RLock()
	ch, ok := b.subs[msg.ToAgent]
	b.mu.RUnlock()
	if !ok {
		return ErrAgentNotRegistered
	}

	select {
	case ch <- msg:
		return nil
	default:
		return ErrAgentQueueFull
	}
}
