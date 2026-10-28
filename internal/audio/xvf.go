package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// XVF streams one channel of the XVF3800's 6-channel USB capture from the
// host it is plugged into, over ssh, or from this machine's ALSA when Host
// is empty. Channels are numbered 1–6 as in the channel map: 1 processed,
// 2 ASR beam, 3–6 raw mics. Channel 0 opens the PCM in mono instead, ALSA
// mixing it, so any capture device works.
type XVF struct {
	Host    string        // ssh destination, e.g. micbox; "" reads this machine's ALSA
	Device  string        // ALSA PCM on the host, e.g. hw:Array,0
	Channel int           // 1..Channels; 0 records the PCM in mono
	CtlPath string        // ssh ControlPath of the persistent mux master
	stall   time.Duration // tests shorten the stdout deadline
	lease   func()
	Tail    time.Duration // kept streaming after Stop: the last syllable is in flight

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   bytes.Buffer
	stopping atomic.Bool
	endOnce  sync.Once
	done     chan struct{} // closed when the command has exited
	errMu    sync.Mutex
	err      error
}

// Input is the PCM arecord records, on Host over ssh or on this machine.
func (x *XVF) Input() TakeInput {
	in := TakeInput{Source: "local", PCM: x.Device, Channel: x.Channel}
	if x.Host != "" {
		in.Source, in.Host = "ssh", x.Host
	}
	return in
}

// Channels is the XVF3800 16k6ch firmware's channel count.
const Channels = 6

// remote runs arecord in the background and ends it when the shell's stdin
// closes, so one connection both carries the stream and stops it: SIGINT
// makes arecord flush and exit, and the device is free for the next start.
// arecord writes 20 ms periods (-F, µs) from a 200 ms buffer (-B): its
// default 125 ms period is audio a stop would wait for.
func (x *XVF) remote() string {
	return fmt.Sprintf("arecord -q -D %s -f S16_LE -r %d -c %d -t raw -F 20000 -B 200000 - & cat >/dev/null; kill -INT $! 2>/dev/null; wait",
		x.Device, Rate, x.channels())
}

// channels is how many channels arecord opens: six to pick one, one for mono.
func (x *XVF) channels() int {
	if x.Channel == 0 {
		return 1
	}
	return Channels
}

// command runs remote over ssh on Host, or in sh on this machine.
func (x *XVF) command() (string, []string) {
	if x.Host == "" {
		return "sh", []string{"-c", x.remote()}
	}
	return "ssh", x.sshArgs()
}

// sshArgs keeps one mux master per ControlPath, so a start reuses the open
// connection instead of a new handshake, and the master dies with the link
// (ServerAlive).
func (x *XVF) sshArgs() []string {
	return []string{
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + x.CtlPath, "-o", "ControlPersist=yes",
		"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2",
		x.Host, x.remote(),
	}
}

func (x *XVF) Start(ctx context.Context) (<-chan []int16, error) {
	if x.Channel < 0 || x.Channel > Channels {
		return nil, fmt.Errorf("xvf: channel %d outside 0..%d", x.Channel, Channels)
	}
	lease, err := xvfLease(ctx, x.Host+"\x00"+x.Device)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			lease()
		}
	}()
	x.lease = lease
	name, args := x.command()
	x.cmd = exec.CommandContext(ctx, name, args...)
	// A cancelled context ends the stream the way Stop does, through the
	// shell's stdin, and the hard kill takes the process group: killing the
	// shell alone would orphan arecord, which keeps the PCM and the stdout
	// pipe, so the stream would never end.
	x.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	x.cmd.Cancel = func() error { x.end(); return nil }
	x.cmd.Stderr = &x.stderr
	if x.stdin, err = x.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := x.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	x.done = make(chan struct{})
	if err := x.cmd.Start(); err != nil {
		return nil, fmt.Errorf("xvf: %s: %w", name, err)
	}
	started = true
	ch := make(chan []int16, 256)
	var last atomic.Int64
	began := time.Now()
	go x.watch(out, began, &last)
	go x.pumpRead(out, ch, func() { last.Store(max(1, int64(time.Since(began)))) })
	return ch, nil
}

func (x *XVF) pump(r io.Reader, ch chan<- []int16) { x.pumpRead(r, ch, func() {}) }

func (x *XVF) pumpRead(r io.Reader, ch chan<- []int16, arrived func()) {
	defer close(ch)
	if x.lease != nil {
		defer x.lease()
	}
	d := Deinterleaver{Channels: x.channels(), Channel: max(x.Channel, 1)}
	buf := make([]byte, Rate*2*x.channels()/50) // 20 ms of frames
	var total int
	for {
		n, err := r.Read(buf)
		if n > 0 {
			arrived()
			total += n
			if s := d.Push(buf[:n]); len(s) > 0 {
				ch <- s
			}
		}
		if err != nil {
			break
		}
	}
	werr := x.cmd.Wait()
	close(x.done)
	msg := strings.TrimSpace(x.stderr.String())
	x.errMu.Lock()
	defer x.errMu.Unlock()
	switch {
	case x.err != nil:
	case total == 0 && (msg != "" || werr != nil):
		x.err = x.unavailable(firstNonEmpty(msg, fmt.Sprint(werr)))
	case sshFailure(werr):
		x.err = fmt.Errorf("stream cut: %v %s", werr, msg)
	case x.stopping.Load():
	case werr != nil:
		x.err = fmt.Errorf("stream cut: %v %s", werr, msg)
	default:
		x.err = errors.New("stream cut")
	}
}

// unavailable is the error of a stream that never delivered: arecord's
// message, or, when it could not open six channels, the rule it broke.
func (x *XVF) unavailable(msg string) error {
	if x.Channel > 0 && strings.Contains(msg, "Channels count non available") {
		return fmt.Errorf("%s is not a 6-channel PCM: channel %d picks one of the XVF3800's six; any other device records with channel 0 (mono)", x.Device, x.Channel)
	}
	return fmt.Errorf("array unavailable: %s", msg)
}

// Stop keeps the stream open for Tail, then ends it (end); the channel
// closes once the rest has arrived.
func (x *XVF) Stop() {
	if x.cmd == nil || x.stopping.Swap(true) {
		return
	}
	time.Sleep(x.Tail)
	x.end()
}

// end closes the shell's stdin, which ends arecord, and kills the command's
// process group if it has not exited 3 s later.
func (x *XVF) end() {
	x.endOnce.Do(func() {
		_ = x.stdin.Close()
		time.AfterFunc(3*time.Second, func() {
			select {
			case <-x.done:
			default:
				_ = syscall.Kill(-x.cmd.Process.Pid, syscall.SIGKILL)
			}
		})
	})
}

func (x *XVF) Err() error {
	x.errMu.Lock()
	defer x.errMu.Unlock()
	return x.err
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

var xvfInputs sync.Map

func xvfLease(ctx context.Context, key string) (func(), error) {
	v, _ := xvfInputs.LoadOrStore(key, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (x *XVF) watch(out io.ReadCloser, began time.Time, last *atomic.Int64) {
	stall := x.stall
	if stall <= 0 {
		stall = RemoteStall
	}
	tick := time.NewTicker(stall / 4)
	defer tick.Stop()
	for {
		select {
		case <-x.done:
			return
		case <-tick.C:
			if time.Since(began)-time.Duration(last.Load()) < stall {
				continue
			}
			if !x.stopping.Load() {
				x.errMu.Lock()
				if x.err == nil {
					x.err = fmt.Errorf("array %s: no audio for %v", x.Device, stall)
				}
				x.errMu.Unlock()
			}
			x.end()
			out.Close()
			return
		}
	}
}

func sshFailure(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 255
}
