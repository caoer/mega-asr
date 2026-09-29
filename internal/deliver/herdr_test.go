package deliver

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHerdr answers one request on a unix socket with reply and returns the
// request it read.
func fakeHerdr(t *testing.T, reply string) (sock string, got <-chan map[string]any) {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr") // a unix socket path must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		var req map[string]any
		json.Unmarshal(line, &req)
		ch <- req
		conn.Write([]byte(reply + "\n"))
	}()
	return sock, ch
}

// A long take goes to herdr as one pane.send_input request carrying the whole
// text and no keys, so herdr can frame it as a single bracketed paste.
func TestSendInputWholeTextNoKeys(t *testing.T) {
	sock, got := fakeHerdr(t, `{"id":"megavoice","result":{"type":"ok"}}`)
	text := strings.Repeat("花园里的番茄 ripened early this year, so 我们 picked them. ", 105) // ~7 KB, mixed zh/en
	if err := sendInput(sock, "w1:p1", text); err != nil {
		t.Fatal(err)
	}
	req := <-got
	params, _ := req["params"].(map[string]any)
	if req["method"] != "pane.send_input" || params["pane_id"] != "w1:p1" || params["text"] != text {
		t.Fatalf("request = method %v pane %v text %d bytes, want pane.send_input w1:p1 %d bytes",
			req["method"], params["pane_id"], len(params["text"].(string)), len(text))
	}
	if _, ok := params["keys"]; ok {
		t.Errorf("request carries keys %v; Enter belongs to Submit", params["keys"])
	}
}

func TestSendInputError(t *testing.T) {
	sock, _ := fakeHerdr(t, `{"id":"megavoice","error":{"code":"pane_not_found","message":"no pane w9:p9"}}`)
	err := sendInput(sock, "w9:p9", "hi")
	if err == nil || !strings.Contains(err.Error(), "pane_not_found") {
		t.Fatalf("err = %v, want pane_not_found", err)
	}
}
