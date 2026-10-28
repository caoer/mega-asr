// Package update checks a staging release feed on a private ccc-pages page,
// remembers which versions the user skipped, and installs an update by
// download, sha256 check and rename-swap. It has no UI and no cgo: the app
// asks the user (更新 / 跳过此版本 / 稍后) and relaunches itself.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Interval is how often an app checks after the check at launch.
const Interval = 24 * time.Hour

// Config is what an app bakes in at build time plus where it keeps state.
type Config struct {
	// Channel is the build's release channel ("staging"). Empty — a dev
	// build — disables the updater: no request is ever made.
	Channel string
	// FeedURL is the release record, https://<host>/d/<slug>/release.<app>?raw=1.
	// The page origin and slug for the unlock come from it.
	FeedURL string
	// Token is the page's viewer token for this build, spent by POST /unlock.
	Token string
	// Current is the installed version, v0.MINOR.PATCH.
	Current string
	// StateDir holds state.json: the skip list and the last feed with its ETag.
	StateDir string
	// OSVersion is the running macOS version ("15.1"); empty reads it from
	// the kernel. A release whose min_macos is above it is not offered.
	OSVersion string
	// HTTPClient is the client to copy; nil is a zero http.Client. The
	// updater sets its own cookie jar on the copy.
	HTTPClient *http.Client
	// Extract unpacks a verified download into dir and returns the path of
	// the .app inside it; nil is the macOS default (zip by ditto, dmg by
	// hdiutil + ditto).
	Extract func(ctx context.Context, archive, dir string) (string, error)
}

// Release is the feed record.
type Release struct {
	Version  string `json:"version"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Notes    string `json:"notes"`    // Chinese, the language every app reads
	NotesEN  string `json:"notes_en"` // English; an app shows Notes when it is empty
	MinMacOS string `json:"min_macos"`
}

// Errors an app maps to what the user sees; UserMessage does the mapping.
// Any other error from Check or Apply is transient: say nothing, try again
// next cycle.
var (
	// ErrRevoked: the baked token no longer unlocks the page. The updater
	// stops polling until the app restarts.
	ErrRevoked = errors.New("update: app token revoked")
	// ErrChecksum: the download did not match the feed's sha256; it was
	// discarded and the installed app is untouched.
	ErrChecksum = errors.New("update: sha256 mismatch")
	// ErrNotWritable: the app's folder cannot be written; the installed app
	// is untouched. Open the release page so the user reinstalls.
	ErrNotWritable = errors.New("update: app folder not writable")
	// ErrDisabled: Apply on a build with no Channel.
	ErrDisabled = errors.New("update: disabled (no channel)")
)

// UserMessage is the line to show for err, or "" when the user sees nothing.
func UserMessage(err error) string {
	switch {
	case errors.Is(err, ErrRevoked):
		return "更新不可用，请从页面重新安装"
	case errors.Is(err, ErrChecksum):
		return "更新校验失败，下次再试"
	case errors.Is(err, ErrNotWritable):
		return "请从页面重新安装"
	}
	return ""
}

// Updater is safe for concurrent use; overlapping Checks share one request.
type Updater struct {
	cfg     Config
	client  *http.Client
	feed    *url.URL
	origin  string
	slug    string
	current version

	mu       sync.Mutex
	inflight *call
	revoked  bool
	unlocked bool
}

type call struct {
	done chan struct{}
	rel  *Release
	err  error
}

// New validates cfg. With an empty Channel it returns an Updater that never
// makes a request.
func New(cfg Config) (*Updater, error) {
	u := &Updater{cfg: cfg}
	if cfg.Channel == "" {
		return u, nil
	}
	var err error
	if u.current, err = parseVersion(cfg.Current); err != nil {
		return nil, fmt.Errorf("update: current version: %w", err)
	}
	if cfg.StateDir == "" {
		return nil, errors.New("update: StateDir is empty")
	}
	if u.feed, err = url.Parse(cfg.FeedURL); err != nil {
		return nil, fmt.Errorf("update: feed url: %w", err)
	}
	parts := strings.Split(strings.Trim(u.feed.Path, "/"), "/")
	if u.feed.Scheme != "https" || len(parts) != 3 || parts[0] != "d" || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("update: feed url %q is not https://<host>/d/<slug>/<key>", cfg.FeedURL)
	}
	u.origin = u.feed.Scheme + "://" + u.feed.Host
	u.slug = parts[1]
	if u.client, err = newClient(cfg.HTTPClient); err != nil {
		return nil, err
	}
	return u, nil
}

// Enabled reports whether this build has a channel.
func (u *Updater) Enabled() bool { return u.cfg.Channel != "" }

// Check asks the feed for a release to offer. It returns nil, nil when there
// is none: the build has no channel, the feed's version is not newer than
// Current, the user skipped that version, or it needs a newer macOS.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	if !u.Enabled() {
		return nil, nil
	}
	u.mu.Lock()
	if c := u.inflight; c != nil {
		u.mu.Unlock()
		select {
		case <-c.done:
			return c.rel, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	u.inflight = c
	u.mu.Unlock()

	c.rel, c.err = u.check(ctx)

	u.mu.Lock()
	u.inflight = nil
	u.mu.Unlock()
	close(c.done)
	return c.rel, c.err
}

func (u *Updater) check(ctx context.Context) (*Release, error) {
	u.mu.Lock()
	st, err := u.load()
	u.mu.Unlock()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.feed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if st.Feed != nil && st.ETag != "" {
		req.Header.Set("If-None-Match", st.ETag)
	}
	resp, err := u.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var fresh *Release
	switch resp.StatusCode {
	case http.StatusNotModified:
	case http.StatusOK:
		fresh = &Release{}
		if err := json.NewDecoder(resp.Body).Decode(fresh); err != nil {
			return nil, fmt.Errorf("update: feed: %w", err)
		}
	default:
		return nil, fmt.Errorf("update: feed: %s", resp.Status)
	}
	// re-read under the lock: a Skip may have landed during the request
	u.mu.Lock()
	defer u.mu.Unlock()
	if st, err = u.load(); err != nil {
		return nil, err
	}
	if fresh != nil {
		st.Feed, st.ETag = fresh, resp.Header.Get("ETag")
		if err := u.save(st); err != nil {
			return nil, err
		}
	}
	if st.Feed == nil {
		return nil, errors.New("update: feed: 304 with no stored feed")
	}
	return u.offer(st)
}

// offer applies the version, skip and macOS rules to the stored feed.
func (u *Updater) offer(st *state) (*Release, error) {
	rel := *st.Feed
	v, err := parseVersion(rel.Version)
	if err != nil {
		return nil, fmt.Errorf("update: feed version: %w", err)
	}
	if v.compare(u.current) <= 0 || slices.Contains(st.Skip, v.String()) {
		return nil, nil
	}
	if rel.MinMacOS != "" {
		have := u.cfg.OSVersion
		if have == "" {
			have = osVersion()
		}
		if have != "" && compareDotted(have, rel.MinMacOS) < 0 {
			return nil, nil
		}
	}
	ref, err := url.Parse(rel.URL)
	if err != nil || rel.URL == "" {
		return nil, fmt.Errorf("update: feed url %q", rel.URL)
	}
	rel.URL = u.feed.ResolveReference(ref).String()
	rel.Version = v.String()
	return &rel, nil
}

// Skip records the user's 跳过此版本: Check stays silent about version until a
// newer one is published.
func (u *Updater) Skip(ver string) error {
	if !u.Enabled() {
		return nil
	}
	v, err := parseVersion(ver)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := u.load()
	if err != nil {
		return err
	}
	if !slices.Contains(st.Skip, v.String()) {
		st.Skip = append(st.Skip, v.String())
	}
	return u.save(st)
}

// state is StateDir/state.json.
type state struct {
	ETag string   `json:"etag,omitempty"`
	Feed *Release `json:"feed,omitempty"`
	Skip []string `json:"skip,omitempty"`
}

func (u *Updater) statePath() string { return filepath.Join(u.cfg.StateDir, "state.json") }

// load reads the state; a missing or unreadable file is an empty state, so a
// corrupt file costs one full feed read and the skip list, never the updater.
func (u *Updater) load() (*state, error) {
	st := &state{}
	b, err := os.ReadFile(u.statePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("update: state: %w", err)
	}
	if err == nil && json.Unmarshal(b, st) != nil {
		st = &state{}
	}
	return st, nil
}

func (u *Updater) save(st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(u.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("update: state: %w", err)
	}
	f, err := os.CreateTemp(u.cfg.StateDir, ".state-*")
	if err != nil {
		return fmt.Errorf("update: state: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), u.statePath())
	}
	if werr != nil {
		os.Remove(f.Name())
		return fmt.Errorf("update: state: %w", werr)
	}
	return nil
}

// version is v0.MINOR.PATCH (any major).
type version [3]int

func parseVersion(s string) (version, error) {
	var v version
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("version %q is not vMAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strconv.Itoa(n) != p {
			return v, fmt.Errorf("version %q is not vMAJOR.MINOR.PATCH", s)
		}
		v[i] = n
	}
	return v, nil
}

func (v version) String() string { return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]) }

func (v version) compare(w version) int {
	for i := range v {
		if v[i] != w[i] {
			if v[i] < w[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// compareDotted compares macOS versions such as "15", "14.6.1"; a part that
// is not a number counts as 0.
func compareDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
