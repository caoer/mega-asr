//go:build darwin

package audio

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTapLive records a real take: the apps named in MEGAMEET_TAP_APPS
// (comma-separated bundle ids; empty taps everything) and the mic whose UID
// is MEGAMEET_TAP_MIC (empty: the default input) for MEGAMEET_TAP_SECONDS (default 10), writing mic.wav and remote.wav to
// MEGAMEET_TAP_OUT and logging each track's level per second. It needs the
// System Audio Recording grant, so it runs only when MEGAMEET_TAP_OUT is set.
func TestTapLive(t *testing.T) {
	out := os.Getenv("MEGAMEET_TAP_OUT")
	if out == "" {
		t.Skip("MEGAMEET_TAP_OUT not set")
	}
	logApps := func() {
		apps, err := Apps()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.Out || a.In {
				t.Logf("app pid %d out=%v in=%v %s %s", a.PID, a.Out, a.In, a.Bundle, a.Name)
			}
		}
	}
	logApps()
	secs, _ := strconv.Atoi(os.Getenv("MEGAMEET_TAP_SECONDS"))
	if secs == 0 {
		secs = 10
	}
	tap := &Tap{MicUID: os.Getenv("MEGAMEET_TAP_MIC")}
	var err error
	if s := os.Getenv("MEGAMEET_TAP_APPS"); s != "" {
		tap.Apps = strings.Split(s, ",")
	}
	ctx := context.Background()
	chans := []<-chan []int16{nil, nil}
	for i, src := range []Source{tap.Mic(), tap.Remote()} {
		if chans[i], err = src.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	opened := time.Now()
	t.Logf("device %+v", tap.Info())
	tracks := make([][]int16, 2)
	var wg sync.WaitGroup
	for i := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range chans[i] {
				tracks[i] = append(tracks[i], s...)
			}
		}()
	}
	half := time.Duration(secs) * time.Second / 2
	time.Sleep(half)
	logApps()
	time.Sleep(half)
	tap.Remote().Stop()
	wg.Wait()
	if err := tap.Mic().Err(); err != nil {
		t.Fatal(err)
	}
	info := tap.Info()
	t.Logf("device %+v; first block %v after Start returned", info, info.First.Sub(opened))
	for sec := 0; sec*Rate < len(tracks[0]); sec++ {
		a, b := sec*Rate, min((sec+1)*Rate, len(tracks[0]))
		m, _ := Stats(tracks[0][a:b])
		r, _ := Stats(tracks[1][a:b])
		t.Logf("t=%3d  mic %6.1f dBFS  remote %6.1f dBFS", sec, m, r)
	}
	for i, name := range []string{"mic.wav", "remote.wav"} {
		if err := SaveWAV(filepath.Join(out, name), tracks[i]); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("frames: mic %d remote %d", len(tracks[0]), len(tracks[1]))
	if len(tracks[0]) != len(tracks[1]) || len(tracks[0]) == 0 {
		t.Fatalf("tracks differ or are empty: mic %d remote %d", len(tracks[0]), len(tracks[1]))
	}
}
