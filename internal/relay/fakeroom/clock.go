package fakeroom

import (
	"sort"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) func() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	t := time.AfterFunc(d, f)
	return t.Stop
}
func RealClock() Clock { return realClock{} }

type virtualTimer struct {
	at      time.Time
	seq     uint64
	f       func()
	stopped bool
}
type VirtualClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers []*virtualTimer
}

func NewVirtualClock(start time.Time) *VirtualClock { return &VirtualClock{now: start} }
func (c *VirtualClock) Now() time.Time              { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *VirtualClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &virtualTimer{at: c.now.Add(d), seq: c.seq, f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if t.stopped {
			return false
		}
		t.stopped = true
		return true
	}
}
func (c *VirtualClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool {
			if c.timers[i].at.Equal(c.timers[j].at) {
				return c.timers[i].seq < c.timers[j].seq
			}
			return c.timers[i].at.Before(c.timers[j].at)
		})
		var t *virtualTimer
		for len(c.timers) > 0 {
			x := c.timers[0]
			c.timers = c.timers[1:]
			if x.stopped {
				continue
			}
			if x.at.After(target) {
				c.timers = append([]*virtualTimer{x}, c.timers...)
				break
			}
			t = x
			c.now = x.at
			break
		}
		if t == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		t.f()
	}
}
