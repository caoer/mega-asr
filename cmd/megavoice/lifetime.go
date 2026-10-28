package main

import (
	"context"
	"sync"
)

// serverLifetime stops and joins the server's owned background senders before
// any normal exit hands control back to launchd.
type serverLifetime struct {
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closed     bool
	wg         sync.WaitGroup
	controller interrupter
}

func (l *serverLifetime) bind(c interrupter) { l.mu.Lock(); l.controller = c; l.mu.Unlock() }

// Interrupt also covers the startup interval before a controller is published.
func (l *serverLifetime) Interrupt() []string {
	l.cancel()
	l.mu.Lock()
	c := l.controller
	l.mu.Unlock()
	if c != nil {
		return c.Interrupt()
	}
	return nil
}

func newServerLifetime() *serverLifetime {
	ctx, cancel := context.WithCancel(context.Background())
	return &serverLifetime{ctx: ctx, cancel: cancel}
}

func (l *serverLifetime) start(run func(context.Context)) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.wg.Add(1)
	l.mu.Unlock()
	go func() { defer l.wg.Done(); run(l.ctx) }()
}

func (l *serverLifetime) close() {
	l.mu.Lock()
	l.closed = true
	l.cancel()
	l.mu.Unlock()
	l.wg.Wait()
}

func (l *serverLifetime) wrapExit(exit func(int)) func(int) {
	var once sync.Once
	return func(code int) { once.Do(func() { l.close(); exit(code) }) }
}
