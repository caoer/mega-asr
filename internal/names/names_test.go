package names

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fictional people.
var people = []Person{
	{Slug: "linxiaoyu", Name: "林晓雨", Aliases: []string{"linxiaoyu", "林晓雨", "晓雨"}, Spoken: []string{"晓雨", "Rain"}},
	{Slug: "yubei", Name: "余贝", Aliases: []string{"yubei", "余贝"}},
}

func TestCandidates(t *testing.T) {
	r := NewRoster(people)
	for _, c := range []struct {
		text string
		want []Candidate
	}{
		// same sound, the full name and the given name; a text with none
		{"昨天林小雨说小雨会来", []Candidate{
			{Span: "林小雨", Form: "林晓雨", Person: "林晓雨", Kind: Same},
			{Span: "小雨", Form: "晓雨", Person: "林晓雨", Kind: Same},
		}},
		{"今天天气不错", nil},
		// one syllable off, sharing its initial: near
		{"晓月说好", []Candidate{{Span: "晓月", Form: "晓雨", Person: "林晓雨", Kind: Near}}},
		// zh/z, -ng/-n fold: 于备 is yu bei; 雨北 overlaps the written 林晓雨
		{"林晓雨北京", nil},
		{"交给于备", []Candidate{{Span: "于备", Form: "余贝", Person: "余贝", Kind: Same}}},
		// a listed form is never a candidate
		{"余贝和晓雨", nil},
	} {
		got := r.Candidates(c.text)
		for i := range got {
			got[i].at = 0
		}
		if (len(got) > 0 || len(c.want) > 0) && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.text, got, c.want)
		}
		if f := Fires(got); f != (len(c.want) > 0 && c.want[0].Kind == Same) {
			t.Errorf("%s: fires %v", c.text, f)
		}
	}
}

func TestSyllableFold(t *testing.T) {
	for _, c := range []struct {
		ch   rune
		want syllable
	}{
		{'张', syllable{"z", "an"}}, {'赞', syllable{"z", "an"}}, {'能', syllable{"l", "en"}},
		{'雨', syllable{"y", "u"}}, {'安', syllable{"", "an"}}, {'兄', syllable{"x", "ong"}},
	} {
		if got := syllableOf(c.ch); got != c.want {
			t.Errorf("%c: %+v, want %+v", c.ch, got, c.want)
		}
	}
}

func TestApply(t *testing.T) {
	got := Apply("小雨说林小雨在", []Edit{{From: "小雨", To: "晓雨"}, {From: "林小雨", To: "林晓雨"}})
	if got != "晓雨说林晓雨在" {
		t.Fatal(got)
	}
}

// jev answers as the endpoint does, with the probability each option key
// gets from p, and keeps what it was sent.
func jev(t *testing.T, p map[string]float64, sent *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k-test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Questions map[string]struct {
				Criteria map[string]string
			}
		}
		json.Unmarshal(b, &body)
		var whole map[string]any
		json.Unmarshal(b, &whole)
		*sent = append(*sent, whole)
		ans := map[string]any{}
		for id, q := range body.Questions {
			probs, best, top := map[string]float64{}, none, 0.0
			for k := range q.Criteria {
				probs[k] = p[k]
				if p[k] > top {
					best, top = k, p[k]
				}
			}
			ans[id] = map[string]any{"type": "choice", "choice": best, "confidence": 0.9, "probabilities": probs}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": ans, "usage": map[string]int{"input_tokens": 100}})
	}))
}

func gate(t *testing.T, url string) Gate {
	dir := t.TempDir()
	path := filepath.Join(dir, "people.json")
	b, _ := json.Marshal(map[string]any{"schema": "people_index@1", "people": people})
	os.WriteFile(path, b, 0o644)
	return Gate{People: path, Key: func() (string, error) { return "k-test", nil },
		Jev: Jev{URL: url, Model: Model}, Threshold: 0.5, Timeout: time.Second}
}

func TestGate(t *testing.T) {
	var sent []map[string]any
	srv := jev(t, map[string]float64{"林晓雨: 林晓雨": 0.8, "林晓雨: 晓雨": 0.4, none: 0.1}, &sent)
	defer srv.Close()
	g := gate(t, srv.URL)

	// fires: one request, a Choice per occurrence; 0.8 applied, 0.4 not
	r := g.Correct(context.Background(), "昨天林小雨说小雨会来")
	if !r.Fired || r.Err != nil || r.Questions != 3 || len(sent) != 1 {
		t.Fatalf("%+v, %d requests", r, len(sent))
	}
	if r.Text != "昨天林晓雨说小雨会来" || len(r.Edits) != 1 || r.Edits[0].P != 0.8 {
		t.Fatalf("%+v", r)
	}
	q := sent[0]["questions"].(map[string]any)["q0"].(map[string]any)
	if ins := q["instructions"].(map[string]any); ins["span"] != "林小雨" || ins["span_pinyin"] != "lín xiǎo yǔ" ||
		ins["context"] != "昨天【林小雨】说小雨会来" {
		t.Fatalf("instructions %v", ins)
	}
	if st := sent[0]["state"].(map[string]any); st["text"] != "昨天林小雨说小雨会来" || sent[0]["model"] != Model {
		t.Fatalf("state %v", sent[0])
	}

	// a near-sound candidate alone does not fire; ungated it asks
	if r := g.Correct(context.Background(), "晓月说好"); r.Fired || r.Text != "晓月说好" || len(sent) != 1 {
		t.Fatalf("near fired: %+v", r)
	}
	g.Ungated = true
	if r := g.Correct(context.Background(), "晓月说好"); !r.Fired || len(sent) != 2 {
		t.Fatalf("ungated: %+v", r)
	}
}

// A failed request, a slow one or a missing key delivers the text as it was.
func TestGateFailsOpen(t *testing.T) {
	text := "昨天林小雨说"
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer slow.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", 529)
	}))
	defer down.Close()
	g := gate(t, slow.URL)
	g.Timeout = 50 * time.Millisecond
	if r := g.Correct(context.Background(), text); r.Err == nil || r.Text != text || r.Wall > 400*time.Millisecond {
		t.Fatalf("slow: %+v", r)
	}
	g = gate(t, down.URL)
	if r := g.Correct(context.Background(), text); r.Err == nil || !strings.Contains(r.Err.Error(), "529") || r.Text != text {
		t.Fatalf("down: %+v", r)
	}
	g.Key = func() (string, error) { return "", errors.New("no key") }
	if r := g.Correct(context.Background(), text); r.Err == nil || r.Text != text {
		t.Fatalf("no key: %+v", r)
	}
	g.People = filepath.Join(t.TempDir(), "absent.json")
	if r := g.Correct(context.Background(), text); r.Fired || r.Text != text {
		t.Fatalf("no roster: %+v", r)
	}
}

func TestApplyJudgedOccurrenceOnly(t *testing.T) {
	text := "我跟沉沉说了，大家都睡得沉沉的"
	at := strings.Index(text, "沉沉")
	got := Apply(text, []Edit{{At: &at, From: "沉沉", To: "陈晨"}})
	if want := "我跟陈晨说了，大家都睡得沉沉的"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if got := markAt(text, "沉沉", strings.LastIndex(text, "沉沉")); !strings.Contains(got, "睡得【沉沉】的") {
		t.Fatal(got)
	}
}
