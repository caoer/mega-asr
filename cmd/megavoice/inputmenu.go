package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/caoer/mega-asr/internal/app"
)

// inputView is an input the Input menu offers: audio.Input as the menu
// reads it.
type inputView struct {
	UID, Name string
	Channels  int
	Default   bool // the system's default input
}

// nextTake is the Input menu's footer: the source is read from the file as
// each take starts, so a pick needs no restart.
const nextTake = "Applies at the next take"

// capturePick is the writes that make a local input the source: its UID,
// its name (shown while it is not connected), its channel, then the source,
// last, so every step leaves a valid file.
func capturePick(uid, name string, channel int) [][2]string {
	return [][2]string{
		{"capture.mic", strconv.Quote(uid)},
		{"capture.mic_name", strconv.Quote(name)},
		{"capture.mic_channel", strconv.Itoa(channel)},
		{"capture.source", `"local"`},
	}
}

// picksLocal is whether sets make a local input the source: such a pick
// asks for the Microphone grant as it is made, since the ssh source never
// asked at launch and a take opened under the prompt records silence.
func picksLocal(sets [][2]string) bool {
	for _, kv := range sets {
		if kv == [2]string{"capture.source", `"local"`} {
			return true
		}
	}
	return false
}

// remoteView is a paired remote mic as the Input menu shows it.
type remoteView struct {
	Name, Addr string
	Offline    bool // the probe as the menu opened did not reach it
}

// foundView is a mic server Add Remote Mic… offers: advertised on the
// network, or an ssh config Host that answers.
type foundView struct {
	Name       string // the ssh Host that reaches it, else the name it advertises
	Advertised string // the name it advertises on mDNS, when that is not Name
	Addr       string // host:port its stream dials
	SSH        string // the ssh Host that reaches it; "" pairs by PIN
}

// remotePick is the writes that make a paired remote mic the source: the
// remote, then the source, so every step leaves a valid file.
func remotePick(name string) [][2]string {
	return [][2]string{{"capture.remote", strconv.Quote(name)}, {"capture.source", `"remote"`}}
}

func (v voiceState) remote(name string) *remoteView {
	for i := range v.Remotes {
		if v.Remotes[i].Name == name {
			return &v.Remotes[i]
		}
	}
	return nil
}

// remoteLabel names a remote mic: "micbox (remote mic)", offline when the
// probe did not reach it, not paired when remotes.toml lacks it.
func (v voiceState) remoteLabel(name string) string {
	switch r := v.remote(name); {
	case r == nil:
		return name + " (remote mic, not paired)"
	case r.Offline:
		return name + " (remote mic, offline)"
	}
	return name + " (remote mic)"
}

// inputName is the input takes record from, as the Input row names it.
func (v voiceState) inputName() string {
	c := v.Capture
	switch c.Source {
	case "ssh":
		return sshLabel(c)
	case "remote":
		return v.remoteLabel(c.Remote)
	}
	name := v.pinnedName()
	if c.Mic == "default" {
		name = "System default"
		if d := v.defaultInput(); d != nil {
			name += " (" + d.Name + ")"
		}
	} else if v.input(c.Mic) == nil {
		name += " (not connected)"
	}
	if c.MicChannel > 0 {
		name += fmt.Sprintf(", channel %d", c.MicChannel)
	}
	return name
}

func sshLabel(c app.CaptureConfig) string { return "Array over ssh (" + c.Host + ")" }

// pinnedName is the pinned input's name: the connected input's, else the
// name the pick wrote, else its UID.
func (v voiceState) pinnedName() string {
	if in := v.input(v.Capture.Mic); in != nil {
		return in.Name
	}
	if v.Capture.MicName != "" {
		return v.Capture.MicName
	}
	return v.Capture.Mic
}

func (v voiceState) input(uid string) *inputView {
	for i := range v.Inputs {
		if v.Inputs[i].UID == uid {
			return &v.Inputs[i]
		}
	}
	return nil
}

func (v voiceState) defaultInput() *inputView {
	for i := range v.Inputs {
		if v.Inputs[i].Default {
			return &v.Inputs[i]
		}
	}
	return nil
}

// addRows is Add Remote Mic…: the boxes found, each paired on its pick
// (over ssh, else by PIN; a paired one pairs again), and Other… for a
// typed host.
func (v voiceState) addRows() row {
	ok := v.FileErr == ""
	var sub []row
	for _, f := range v.Found {
		var how []string
		if f.Advertised != "" {
			how = append(how, "advertised as "+f.Advertised)
		}
		if v.remote(f.Name) != nil {
			how = append(how, "paired")
		}
		if f.SSH == "" {
			how = append(how, "PIN")
		}
		title := f.Name
		if how != nil {
			title += " (" + strings.Join(how, ", ") + ")"
		}
		sub = append(sub, row{title: title, enabled: ok, act: &act{kind: actPair, arg: f.Addr, key: f.SSH}})
	}
	switch {
	case v.Scanning:
		sub = append(sub, row{title: "Searching…"})
	case v.Found == nil:
		sub = append(sub, row{title: "No mic server found"})
	}
	if v.FoundNote != "" {
		sub = append(sub, row{title: "⚠︎ " + trim(v.FoundNote, 90), color: "orange"})
	}
	sub = append(sub, row{sep: true}, row{title: "Other…", enabled: ok, act: &act{kind: actAddRemote}})
	return row{title: "Add Remote Mic…", enabled: true, sub: sub}
}

// inputRows is the Input submenu: the system default, each input of this
// Mac (a channel submenu when it has more than two), a pinned input that is
// not connected, the ssh array when the file names one, and the paired
// remote mics with Add and Forget.
func (v voiceState) inputRows() row {
	c, ok := v.Capture, v.FileErr == ""
	local := c.Source == "local"
	pick := func(title string, on bool, sets [][2]string) row {
		return row{title: title, enabled: ok, checked: on, act: &act{kind: actCapture, sets: sets}}
	}
	def := "System default"
	if d := v.defaultInput(); d != nil {
		def += " (" + d.Name + ")"
	}
	sub := []row{pick(def, local && c.Mic == "default", capturePick("default", "", 0)), {sep: true}}
	for _, in := range v.Inputs {
		on := local && c.Mic == in.UID
		if in.Channels <= 2 {
			sub = append(sub, pick(in.Name, on, capturePick(in.UID, in.Name, 0)))
			continue
		}
		chans := []row{pick("All channels, mixed", on && c.MicChannel == 0, capturePick(in.UID, in.Name, 0)), {sep: true}}
		for n := 1; n <= in.Channels; n++ {
			chans = append(chans, pick(fmt.Sprintf("Channel %d", n), on && c.MicChannel == n, capturePick(in.UID, in.Name, n)))
		}
		sub = append(sub, row{title: in.Name, enabled: true, checked: on, sub: chans})
	}
	if c.Mic != "default" && v.input(c.Mic) == nil {
		sub = append(sub, row{title: v.pinnedName() + " (not connected)", checked: local})
	}
	if c.Host != "" && c.Device != "" {
		sub = append(sub, row{sep: true}, pick(sshLabel(c), c.Source == "ssh", [][2]string{{"capture.source", `"ssh"`}}))
	}
	sub = append(sub, row{sep: true})
	var forget []row
	for _, r := range v.Remotes {
		sub = append(sub, pick(v.remoteLabel(r.Name), c.Source == "remote" && c.Remote == r.Name, remotePick(r.Name)))
		forget = append(forget, row{title: r.Name, enabled: true, act: &act{kind: actForget, arg: r.Name}})
	}
	if c.Source == "remote" && v.remote(c.Remote) == nil {
		sub = append(sub, row{title: v.remoteLabel(c.Remote), checked: true})
	}
	sub = append(sub, v.addRows())
	if forget != nil {
		sub = append(sub, row{title: "Forget Remote Mic", enabled: ok, sub: forget})
	}
	sub = append(sub, row{sep: true}, row{title: nextTake})
	return row{title: "Input: " + v.inputName(), enabled: true, sub: sub}
}
