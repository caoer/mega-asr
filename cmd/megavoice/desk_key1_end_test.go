package main

import (
	"testing"
	"time"

	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/deliver"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// A stop gesture belongs to the recording seen at down. Ending it through
// another control must not turn the release into a new recording, even
// when its tap was already queued for dispatch.
func TestDeskKey1ReleaseAfterAnotherEnd(t *testing.T) {
	for _, end := range []string{"cancel", "enter", "auto"} {
		for _, queued := range []bool{false, true} {
			name := end + "/before up"
			if queued {
				name = end + "/before dispatch"
			}
			t.Run(name, func(t *testing.T) {
				d, link := newTestDesk(t)
				router := d.router(deskMac{}, deliver.Telegram{})
				ctrl := &session.Controller{NewSource: func() audio.Source { return &heldMacInput{} }, ASR: phoneEngine{}, Deliver: router, UI: phoneUI{}, Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: time.Minute, OnView: d.view}
				if end == "auto" {
					ctrl.MaxDuration = 100 * time.Millisecond
				}
				queue := false
				var pending func()
				d.bind(ctrl, 400*time.Millisecond, func(at time.Time, target *deliver.TelegramTarget, recording string) {
					act := func() {
						ctrl.ToggleKeypadAt(at, func() session.Target { return router.CaptureKeypad(target) }, recording)
					}
					if queue {
						pending = act
					} else {
						act()
					}
				})
				at := time.Now()
				d.key1At(true, at, link.get())
				d.key1At(false, at.Add(100*time.Millisecond), link.get())
				if _, _, ok := ctrl.Recording(); !ok {
					t.Fatal("initial take did not start")
				}
				d.key1At(true, at.Add(time.Second), link.get())
				if queued {
					queue = true
					d.key1At(false, at.Add(time.Second+100*time.Millisecond), link.get())
					if pending == nil {
						t.Fatal("tap was not queued before the other end")
					}
				}
				switch end {
				case "cancel":
					ctrl.Cancel()
				case "enter":
					ctrl.Enter()
				case "auto":
					for deadline := time.Now().Add(2 * time.Second); ; {
						if _, _, recording := ctrl.Recording(); !recording {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("auto end did not stop the take")
						}
						time.Sleep(time.Millisecond)
					}
				}
				if queued {
					pending()
				} else {
					d.key1At(false, at.Add(time.Second+100*time.Millisecond), link.get())
				}
				if id, _, recording := ctrl.Recording(); recording {
					t.Errorf("key1 up restarted recording %s after %s", id, end)
					ctrl.Cancel()
				}
				ctrl.Wait()
			})
		}
	}
}

func TestDeskQueuedKey1LeavesReplacementRecording(t *testing.T) {
	d, link := newTestDesk(t)
	router := d.router(deskMac{}, deliver.Telegram{})
	ctrl := &session.Controller{NewSource: func() audio.Source { return &heldMacInput{} }, ASR: phoneEngine{}, Deliver: router, UI: phoneUI{}, Store: store.Store{Dir: t.TempDir()}, ChunkDir: t.TempDir(), WholeMax: time.Minute, OnView: d.view}
	queue := false
	var pending func()
	d.bind(ctrl, 400*time.Millisecond, func(at time.Time, target *deliver.TelegramTarget, recording string) {
		act := func() {
			ctrl.ToggleKeypadAt(at, func() session.Target { return router.CaptureKeypad(target) }, recording)
		}
		if queue {
			pending = act
		} else {
			act()
		}
	})
	at := time.Now()
	d.key1At(true, at, link.get())
	d.key1At(false, at.Add(100*time.Millisecond), link.get())
	d.key1At(true, at.Add(time.Second), link.get())
	queue = true
	d.key1At(false, at.Add(time.Second+100*time.Millisecond), link.get())
	ctrl.Cancel()
	replacement, err := ctrl.StartFrom("tap")
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("no queued stop gesture")
	}
	pending()
	if id, _, recording := ctrl.Recording(); !recording || id != replacement {
		t.Errorf("stale key1 gesture changed replacement recording: %q %v", id, recording)
	}
	ctrl.Cancel()
	ctrl.Wait()
}
