package meeting

import (
	"sort"
	"time"
)

// MinOverlap is the shortest wall-clock overlap that pairs a local
// recording with a Feishu minute.
const MinOverlap = 30 * time.Second

// IsLocal is a recording megameet transcribes itself and Feishu can score.
func IsLocal(r Record) bool { return r.Source == "mac" || r.Source == "room" }

// Overlap is how long two records ran at the same time on the wall clock.
func Overlap(a, b Record) time.Duration {
	lo, hi := a.Started, a.End()
	if b.Started.After(lo) {
		lo = b.Started
	}
	if e := b.End(); e.Before(hi) {
		hi = e
	}
	if !hi.After(lo) {
		return 0
	}
	return hi.Sub(lo)
}

// Job is one alignment: a local recording scored against a Feishu minute.
type Job struct {
	Local, Feishu Record
	Others        []string // further Feishu minutes overlapping Local: flagged, not used
}

// scoreable is a Feishu minute whose media and transcript have landed.
func scoreable(r Record) bool {
	if r.Source != "feishu" {
		return false
	}
	switch r.State {
	case Uploaded, Processing, Aligned, Ingested:
	default:
		return false
	}
	_, media := r.File("media")
	_, text := r.File("feishu-transcript")
	return media && text
}

// minutes are the scoreable Feishu minutes overlapping local, longest
// overlap first.
func minutes(local Record, all []Record) []Record {
	var out []Record
	for _, r := range all {
		if scoreable(r) && Overlap(local, r) >= MinOverlap {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := Overlap(local, out[i]), Overlap(local, out[j])
		if a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Jobs are the alignments the claimed record owes. Align runs on whichever
// of a pair is claimed second: a local recording aligns against the Feishu
// minute it overlaps longest, if one has landed; a Feishu minute scores the
// local recordings already aligned or ingested that it is the best minute
// for. A minute skips a recording it has already scored unless redo (a
// realign, or a run by hand). A record of any other source owes nothing.
func Jobs(claimed Record, all []Record, redo bool) []Job {
	job := func(local Record, pool []Record) (Job, bool) {
		ms := minutes(local, pool)
		if len(ms) == 0 {
			return Job{}, false
		}
		j := Job{Local: local, Feishu: ms[0]}
		for _, m := range ms[1:] {
			j.Others = append(j.Others, m.ID)
		}
		return j, true
	}
	switch {
	case IsLocal(claimed):
		if j, ok := job(claimed, all); ok {
			return []Job{j}
		}
	case claimed.Source == "feishu" && scoreable(claimed):
		pool := []Record{claimed}
		for _, r := range all {
			if r.ID != claimed.ID {
				pool = append(pool, r)
			}
		}
		var jobs []Job
		for _, r := range all {
			if !IsLocal(r) || (r.State != Aligned && r.State != Ingested) || Overlap(r, claimed) < MinOverlap {
				continue
			}
			if !redo && r.Scores != nil && r.Scores.Against == claimed.ID {
				continue
			}
			if j, ok := job(r, pool); ok && j.Feishu.ID == claimed.ID {
				jobs = append(jobs, j)
			}
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Local.ID < jobs[j].Local.ID })
		return jobs
	}
	return nil
}
