package main

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/caoer/mega-asr/internal/app"
	"github.com/caoer/mega-asr/internal/audio"
	"github.com/caoer/mega-asr/internal/l10n"
	"github.com/caoer/mega-asr/internal/remote"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// phoneCtrl is what a paired phone drives on the controller.
type phoneCtrl interface {
	StartFrom(trigger string) (string, error)
	StopFrom(via, id string) bool
	SendFrom(via, id string) bool
	CancelFrom(via, id string) bool
	Published() (phase session.State, id string, elapsed time.Duration, owner string)
}

// phoneTakes is remote.Takes over the controller: commands that name the
// state they want, refused in words the phone shows as they are. A start is
// refused while a restart or quit waits, as a tap is, and flashes on the
// overlay so a take started from the phone is seen at the Mac.
type phoneTakes struct {
	ctrl       phoneCtrl
	refuseTake func() bool // takeCommands.refuseStart; nil refuses nothing
	flash      func(msg string)
}

// viaName is how the overlay names where a command came from.
func viaName(via string) string {
	switch via {
	case "watch":
		return l10n.T("手表", "Watch")
	case "keyboard":
		return l10n.T("iPhone 键盘", "iPhone keyboard")
	}
	return "iPhone"
}

func (p phoneTakes) Start(via string) (string, error) {
	if p.refuseTake != nil && p.refuseTake() {
		return "", remote.Refuse("exiting", l10n.F("Mac 上的 %s 正在重启或退出，稍后再按", "%s on the Mac is restarting or quitting; try again shortly", app.Name()))
	}
	id, err := p.ctrl.StartFrom(via)
	switch {
	case errors.Is(err, session.ErrRecording):
		return "", remote.Refuse("recording", l10n.T("Mac 已经在录音", "The Mac is already recording"))
	case errors.Is(err, session.ErrClosing):
		return "", remote.Refuse("exiting", l10n.F("Mac 上的 %s 正在重启或退出，稍后再按", "%s on the Mac is restarting or quitting; try again shortly", app.Name()))
	case err != nil:
		return "", remote.Refuse("start_failed", l10n.T("Mac 没能开始录音：", "The Mac couldn't start recording: ")+err.Error())
	}
	p.flash(viaName(via) + l10n.T("：开始", ": start"))
	return id, nil
}

// StartStream uses the controller's per-take sink, with no Mac focus capture.
func (p phoneTakes) StartStream(src audio.Source, out remote.TextSink, via string) (string, error) {
	if p.refuseTake != nil && p.refuseTake() {
		return "", remote.Refuse("exiting", l10n.F("Mac 上的 %s 正在重启或退出，稍后再按", "%s on the Mac is restarting or quitting; try again shortly", app.Name()))
	}
	ctrl, ok := p.ctrl.(interface {
		StartStream(audio.Source, session.Deliverer, string) (string, error)
	})
	if !ok {
		return "", remote.Refuse("start_failed", l10n.T("Mac 不支持键盘音频", "This Mac doesn't take keyboard audio"))
	}
	sink := &phoneSink{out: out, ready: make(chan struct{})}
	id, err := ctrl.StartStream(src, sink, via)
	sink.take = id
	close(sink.ready)
	switch {
	case errors.Is(err, session.ErrRecording):
		return "", remote.Refuse("recording", l10n.T("Mac 已经在录音", "The Mac is already recording"))
	case errors.Is(err, session.ErrClosing):
		return "", remote.Refuse("exiting", l10n.F("Mac 上的 %s 正在重启或退出，稍后再按", "%s on the Mac is restarting or quitting; try again shortly", app.Name()))
	case err != nil:
		return "", remote.Refuse("start_failed", l10n.T("Mac 没能开始录音：", "The Mac couldn't start recording: ")+err.Error())
	}
	if p.flash != nil {
		p.flash(viaName(via) + l10n.T("：开始", ": start"))
	}
	return id, nil
}

type phoneTarget struct{}

func (phoneTarget) String() string { return l10n.T("iPhone 键盘", "iPhone keyboard") }

type phoneSink struct {
	out   remote.TextSink
	ready chan struct{}
	take  string // published by closing ready after StartStream returns
}

func (p *phoneSink) Capture() session.Target { return phoneTarget{} }
func (p *phoneSink) Deliver(_ session.Target, text string) error {
	<-p.ready
	return p.out.DeliverText(p.take, text)
}
func (*phoneSink) Submit(session.Target) error {
	return errors.New(l10n.T("iPhone 键盘不发送 Mac Enter", "The iPhone keyboard doesn't send the Mac's Enter"))
}

var _ remote.StreamTakes = phoneTakes{}

func (p phoneTakes) Stop(via, take string, send bool) error {
	ok := false
	if send {
		ok = p.ctrl.SendFrom(via, take)
	} else {
		ok = p.ctrl.StopFrom(via, take)
	}
	if !ok {
		return p.notThat(take)
	}
	return nil
}

func (p phoneTakes) Cancel(via, take string) error {
	if !p.ctrl.CancelFrom(via, take) {
		return p.notThat(take)
	}
	return nil
}

// notThat says why a stop or cancel of take ended nothing: no take records,
// another one does (the phone pressed on a state the Mac has left), or it is
// a phone keyboard's stream take, which only that keyboard ends.
func (p phoneTakes) notThat(take string) error {
	_, id, _, owner := p.ctrl.Published()
	switch {
	case id != "" && id != take:
		return remote.Refuse("other_take", l10n.T("Mac 上正在录的是另一段，这次没有结束它", "The Mac is recording another take; this one was not ended"))
	case id != "" && owner == session.OwnerStream:
		return remote.Refuse("other_take", remote.ReasonStreamTake())
	}
	return remote.Refuse("not_recording", l10n.T("Mac 没有在录音", "The Mac isn't recording"))
}

// State is one read of the controller's published snapshot, so phase, take
// and owner always agree.
func (p phoneTakes) State() remote.State {
	phase, id, d, owner := p.ctrl.Published()
	return remote.State{Phase: phase.String(), Take: id, ElapsedMS: d.Milliseconds(), Owner: owner}
}

// recentTakes is the last n takes of the store at dir as a hello or a reply
// carries them (relay/PROTOCOL.md § 8): final empty while a take is open.
func recentTakes(dir string, n int) []remote.Recent {
	recs, err := store.Store{Dir: dir}.Latest(n)
	if err != nil {
		return nil
	}
	out := make([]remote.Recent, 0, len(recs))
	for _, r := range recs {
		rc := remote.Recent{Take: filepath.Base(r.Base), AtMS: r.Start().UnixMilli()}
		if st := store.State(r.Events); st.Final() {
			rc.Final, rc.Why = st.State, st.Why
		}
		out = append(out, rc)
	}
	return out
}

// takeDone tells the phones a take's final state from its record, once the
// controller has written it.
func takeDone(r *remote.Server, base string) {
	evs, err := store.Read(base)
	if err != nil {
		return
	}
	if st := store.State(evs); st.Final() {
		r.Done(filepath.Base(base), st.State, st.Why)
	}
}
