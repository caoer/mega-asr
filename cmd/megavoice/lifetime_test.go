package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestServerExitCancelsAndJoinsOwnedSender(t *testing.T) {
	for _, code := range []int{0, 75} {
		t.Run(time.Duration(code).String(), func(t *testing.T) {
			life := newServerLifetime()
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			life.start(func(ctx context.Context) { close(started); <-ctx.Done(); close(cancelled); <-release })
			<-started
			exited := make(chan int, 1)
			go life.wrapExit(func(code int) { exited <- code })(code)
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("sender context not cancelled")
			}
			select {
			case <-exited:
				t.Fatal("exit did not join its sender")
			default:
			}
			close(release)
			select {
			case got := <-exited:
				if got != code {
					t.Fatal(got)
				}
			case <-time.After(time.Second):
				t.Fatal("exit did not finish")
			}
			ran := make(chan struct{}, 1)
			life.start(func(context.Context) { ran <- struct{}{} })
			select {
			case <-ran:
				t.Fatal("closed server accepted a new sender")
			case <-time.After(5 * time.Millisecond):
			}
		})
	}
}

func TestServerSignalDuringStartupJoinsSender(t *testing.T) {
	life := newServerLifetime()
	defer life.close()
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	life.start(func(ctx context.Context) { close(started); <-ctx.Done(); close(cancelled); <-release })
	<-started
	sig := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go onSignal(sig, life, time.Second, life.wrapExit(func(code int) { exited <- code }))
	sig <- syscall.SIGTERM
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("startup sender was not cancelled")
	}
	select {
	case <-exited:
		t.Fatal("signal exit skipped join")
	default:
	}
	close(release)
	select {
	case code := <-exited:
		if code != 75 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("signal exit did not finish")
	}
}
