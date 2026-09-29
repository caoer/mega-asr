package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/caoer/mega-asr/internal/ctl"
	"github.com/caoer/mega-asr/internal/takes"
)

// keyFile is the key of the loopback listener, which the Takes page's
// address carries.
func keyFile() takes.KeyFile { return takes.KeyFile(filepath.Join(stateDir(), "takes.key")) }

// guard holds everything served on ln to a page megavoice opened.
func guard(next http.Handler, ln net.Listener) http.Handler {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return &takes.Guard{Next: next, Key: keyFile().Load, Port: port, Policy: takes.Policy()}
}

// keyedURL is path on the listener at addr with the key; without a key it
// is the bare address, which answers where to open it.
func keyedURL(addr, path string) string {
	key, err := keyFile().Load()
	if err != nil {
		log.Printf("takes: %v", err)
		return "http://" + addr + path
	}
	return takes.URL(addr, path, key)
}

// pageURL is the menu's Takes page address, with the key, on the listener
// this process holds at addr; with none held, no address and why the row
// is greyed.
func pageURL(addr string, err error) (page, note string) {
	switch {
	case addr != "":
		return keyedURL(addr, "/takes/"), ""
	case errors.Is(err, syscall.EADDRINUSE):
		return "", "端口被别的程序占用，录音历史打不开"
	case err != nil:
		return "", "录音历史打不开：" + err.Error()
	}
	return "", ""
}

// takesCmd is `megavoice takes url [--rotate]`: the Takes page's address,
// with the key, on the listener the running megavoice serve holds. With no
// listener held it hands out no key and says why; --rotate writes a new key
// first either way.
func takesCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "url" {
		fmt.Fprintln(os.Stderr, "usage: megavoice takes url [--rotate]")
		return exitCode(2)
	}
	fs := flag.NewFlagSet("takes url", flag.ContinueOnError)
	rotate := fs.Bool("rotate", false, "write a new key; every page opened before is refused")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return exitCode(2)
	}
	if *rotate {
		if _, err := keyFile().Rotate(); err != nil {
			return err
		}
	}
	addr, err := runningListener()
	if err != nil {
		return fmt.Errorf("no address handed out: %w", err)
	}
	key, err := keyFile().Load()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, takes.URL(addr, "/takes/", key))
	return nil
}

// runningListener asks the running megavoice serve for the loopback
// listener it holds.
func runningListener() (string, error) {
	resp, err := ctl.Call(sockPath(), ctl.Request{Cmd: "listener"}, 5*time.Second)
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", errors.New(resp.Error)
	}
	var l struct{ Addr string }
	if err := json.Unmarshal(resp.Data, &l); err != nil || l.Addr == "" {
		return "", fmt.Errorf("megavoice serve names no listener: %s", resp.Data)
	}
	return l.Addr, nil
}
