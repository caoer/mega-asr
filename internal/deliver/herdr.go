package deliver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Herdr drives the herdr CLI.
type Herdr struct{ Bin string }

// herdrWait bounds each herdr call: a run of the CLI, and a dial of its API
// socket with the request and its answer.
var herdrWait = 3 * time.Second

func (h Herdr) run(args ...string) ([]byte, error) {
	return h.runCtx(context.Background(), args...)
}

// runCtx runs the CLI until it exits, herdrWait passes or ctx ends.
func (h Herdr) runCtx(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, herdrWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("herdr %s: %v %s", args[0]+" "+args[1], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Pane is one of herdr's panes as a resend picker lists it.
type Pane struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Workspace   string `json:"workspace"` // the workspace's label
	Title       string `json:"title,omitempty"`
	Agent       string `json:"agent,omitempty"` // the agent herdr sees in it
	Cwd         string `json:"cwd,omitempty"`   // the foreground process's folder
	Focused     bool   `json:"focused,omitempty"`
}

// Panes lists herdr's panes, each with its workspace's label.
func (h Herdr) Panes() ([]Pane, error) {
	out, err := h.run("pane", "list")
	if err != nil {
		return nil, err
	}
	var pl struct {
		Result struct {
			Panes []struct {
				PaneID        string `json:"pane_id"`
				WorkspaceID   string `json:"workspace_id"`
				Focused       bool   `json:"focused"`
				Title         string `json:"title"`
				TerminalTitle string `json:"terminal_title_stripped"`
				Agent         string `json:"agent"`
				Cwd           string `json:"cwd"`
				ForegroundCwd string `json:"foreground_cwd"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &pl); err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	out, err = h.run("workspace", "list")
	if err != nil {
		return nil, err
	}
	var wl struct {
		Result struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Label       string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &wl); err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	labels := map[string]string{}
	for _, w := range wl.Result.Workspaces {
		labels[w.WorkspaceID] = w.Label
	}
	panes := make([]Pane, 0, len(pl.Result.Panes))
	for _, p := range pl.Result.Panes {
		q := Pane{ID: p.PaneID, WorkspaceID: p.WorkspaceID, Workspace: labels[p.WorkspaceID], Title: p.Title,
			Agent: p.Agent, Cwd: p.ForegroundCwd, Focused: p.Focused}
		if q.Title == "" {
			q.Title = p.TerminalTitle
		}
		if q.Cwd == "" {
			q.Cwd = p.Cwd
		}
		panes = append(panes, q)
	}
	return panes, nil
}

// Focused returns herdr's focused pane and the label of its workspace.
func (h Herdr) Focused() (pane, workspace string, err error) {
	panes, err := h.Panes()
	if err != nil {
		return "", "", err
	}
	for _, p := range panes {
		if p.Focused {
			return p.ID, p.Workspace, nil
		}
	}
	return "", "", errors.New("herdr: no focused pane")
}

// SendText pastes text into a pane without pressing Enter, through herdr's
// pane.send_input API: herdr wraps it in bracketed-paste markers when the
// pane's program enabled that mode, so the program reads one paste. Raw bytes
// (`herdr pane send-text`) reach the program as ~1 KB tty reads, and Claude
// Code keeps only the reads after the last one above 800 characters.
func (h Herdr) SendText(pane, text string) error {
	sock, err := h.socket(context.Background())
	if err != nil {
		return err
	}
	return sendInput(sock, pane, text)
}

// socket is the herdr server's API socket, as `herdr status server` reports it.
func (h Herdr) socket(ctx context.Context) (string, error) {
	out, err := h.runCtx(ctx, "status", "server")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(out)) {
		if path, ok := strings.CutPrefix(line, "socket:"); ok {
			return strings.TrimSpace(path), nil
		}
	}
	return "", errors.New("herdr status server: no socket line")
}

// sendInput makes one pane.send_input request with text and no keys.
func sendInput(sock, pane, text string) error {
	_, err := request(context.Background(), sock, "pane.send_input", map[string]string{"pane_id": pane, "text": text})
	return err
}

// readPane returns the last lines logical lines of a pane's output as
// text, wrapped lines joined, as herdr's pane.read gives them.
func readPane(ctx context.Context, sock, pane string, lines int) (string, error) {
	res, err := request(ctx, sock, "pane.read", map[string]any{"pane_id": pane, "source": "recent_unwrapped", "lines": lines})
	if err != nil {
		return "", err
	}
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("herdr pane.read: %w", err)
	}
	return r.Read.Text, nil
}

// paneAgent returns the agent herdr sees in a pane ("claude", "codex"), or
// "" for a shell or any other program.
func paneAgent(ctx context.Context, sock, pane string) (string, error) {
	res, err := request(ctx, sock, "pane.get", map[string]string{"pane_id": pane})
	if err != nil {
		return "", err
	}
	var r struct {
		Pane struct {
			Agent string `json:"agent"`
		} `json:"pane"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("herdr pane.get: %w", err)
	}
	return r.Pane.Agent, nil
}

// request makes one request on herdr's API socket and returns its result.
// The dial, the request and its answer end within herdrWait, or when ctx
// ends.
func request(ctx context.Context, sock, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, herdrWait)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("herdr socket: %w", err)
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok { // herdrWait's, or the caller's earlier one
		conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) }) // ctx cancelled before its deadline
	defer stop()
	req, err := json.Marshal(map[string]any{"id": "megavoice", "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("herdr %s: %w", method, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("herdr %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("herdr %s: %w", method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("herdr %s: %s %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// SendKeys presses keys (herdr key names, e.g. "enter") in a pane.
func (h Herdr) SendKeys(pane string, keys ...string) error {
	_, err := h.run(append([]string{"pane", "send-keys", pane}, keys...)...)
	return err
}
