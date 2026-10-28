package names

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The people are made up.
const indexJSON = `{"updated_at":"2020-03-11T06:40:00Z","people":[
 {"slug":"lin-xiaoxi","name":"林小溪（QA）","aliases":["lin-xiaoxi","林小溪","Xiaoxi Lin"],"spoken":["小溪","溪姐"],"heard-context":{"晓希":"小溪"},"pages":[{"slug":"lin-xiaoxi"},{"slug":"xiaoxi"}]},
 {"slug":"zhou-mingyuan","name":"周明远","aliases":["zhou-mingyuan","周明远","明远","Leo"]},
 {"slug":"old-friend","name":"陈一","status":"former"}
]}`

func testIndex(t *testing.T) *Index {
	t.Helper()
	x, err := FromIndex([]byte(indexJSON))
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestFromIndex(t *testing.T) {
	x := testIndex(t)
	if x.Updated != "2020-03-11T06:40:00Z" || len(x.People) != 3 {
		t.Fatalf("%+v", x)
	}
	lin := x.People[0]
	if lin.Name != "林小溪" || !reflect.DeepEqual(lin.Spoken, []string{"小溪", "溪姐"}) || !reflect.DeepEqual(x.Heard[0], map[string]string{"晓希": "小溪"}) {
		t.Fatalf("spoken forms, gloss dropped: %+v %v", lin, x.Heard[0])
	}
	// No spoken forms: the aliases, less the slug and the name.
	if z := x.People[1]; !reflect.DeepEqual(z.Spoken, []string{"明远", "Leo"}) {
		t.Fatalf("alias fallback %+v", z)
	}
	if i, ok := x.Person("xiaoxi"); !ok || i != 0 {
		t.Fatalf("a secondary page's slug finds its person: %v %v", i, ok)
	}
	if y, err := FromIndex([]byte(`{"people":[{"slug":"a","name":"林小溪","heard-context":["晓希"]}]}`)); err != nil || y.Heard[0]["晓希"] != "" {
		t.Fatalf("heard-context as a list: %v %v", y, err)
	}
	if !x.Listed("leo") || x.Listed("晓希") {
		t.Fatal("listed forms fold case; a heard form is not listed")
	}
}

func TestIndexCandidates(t *testing.T) {
	x := testIndex(t)
	got := map[string]Kind{}
	for _, c := range x.Candidates("我跟小西说了，明元也在，晓希呢？小溪没来") {
		got[c.Span+"~"+c.Form] = c.Kind
	}
	want := map[string]Kind{"小西~小溪": Same, "明元~明远": Same, "晓希~小溪": Heard}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	near := x.Candidates("明言来了")
	if len(near) != 1 || near[0].Kind != Near || near[0].Form != "明远" {
		t.Fatalf("one syllable off, same initial: %+v", near)
	}
}

func TestUserMessage(t *testing.T) {
	x := testIndex(t)
	u := x.User("小西来了", []string{"周明远"})
	for _, s := range []string{"PEOPLE\n林小溪: 小溪, 溪姐\n周明远: 明远, Leo\n陈一", "SPEAKERS\n周明远", "TEXT\n小西来了", "CANDIDATES\n小西 ~ 小溪 (林小溪)"} {
		if !strings.Contains(u, s) {
			t.Errorf("user message lacks %q:\n%s", s, u)
		}
	}
	if u := x.User("今天天气好", nil); strings.Contains(u, "SPEAKERS") || !strings.Contains(u, "CANDIDATES\n(none)") {
		t.Errorf("no speakers, no candidates:\n%s", u)
	}
}

func TestGuard(t *testing.T) {
	x := testIndex(t)
	text := "我跟小西说了，晓希也在，Lio 呢，小溪来了，天气不对"
	for _, c := range []struct {
		e    Edit
		want string // "" kept
	}{
		{Edit{From: "小西", To: "小溪", Person: "林小溪"}, ""},
		{Edit{From: "小西", To: "小溪", Person: ""}, ""},                                               // no person named: any listed form
		{Edit{From: "晓希", To: "小溪", Person: "林小溪"}, ""},                                            // heard-context: kept without the sound test
		{Edit{From: "Lio", To: "Leo", Person: "周明远"}, ""},                                          // Latin, one letter off
		{Edit{From: "小西", To: "小溪", Person: "周明远"}, "to is not a listed form of the person named"}, // the wrong person
		{Edit{From: "小西", To: "小希", Person: "林小溪"}, "to is not a listed form of the person named"},
		{Edit{From: "小溪", To: "溪姐", Person: "林小溪"}, "from is itself a listed name"},
		{Edit{From: "天气", To: "明远", Person: "周明远"}, "from does not sound like to"},
		{Edit{From: "小南", To: "小溪", Person: "林小溪"}, "from is not in the text"},
		{Edit{From: "小西", To: "小西", Person: "林小溪"}, "empty or unchanged"},
	} {
		if _, why := x.Guard(text, c.e); why != c.want {
			t.Errorf("%+v: %q, want %q", c.e, why, c.want)
		}
	}
}

func TestSoundsLike(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"小西", "小溪", true},   // same syllables
		{"明言", "明远", true},   // one syllable off
		{"林晓", "林小溪", false}, // different length, not a stutter
		{"小小溪", "小溪", true},  // a stutter
		{"李", "林", false},    // one syllable, different
		{"Lio", "Leo", true},
		{"Lemonade", "Leo", false},
		{"利奥", "Leo", true}, // a Latin name written in Han
		{"Leo", "利奥", false},
	} {
		if got := SoundsLike(c.from, c.to); got != c.want {
			t.Errorf("SoundsLike(%q, %q) = %v", c.from, c.to, got)
		}
	}
}

func TestApplyFires(t *testing.T) {
	got, fires := applyFires("小西和小西姐，还有小西", []Edit{{From: "小西", To: "小溪"}, {From: "小西姐", To: "溪姐"}}, []int{0, 0})
	if got != "小溪和溪姐，还有小溪" || len(fires) != 3 {
		t.Fatalf("longest first, never overlapping: %q %+v", got, fires)
	}
}

func TestParse(t *testing.T) {
	e, err := Parse(`thinking {"edits":[{"from":"a","to":"b","person":"p"}]} then {"edits":[{"from":"c","to":"d","person":"q"}]}`)
	if err != nil || len(e) != 1 || e[0].From != "c" {
		t.Fatalf("the last edits object: %+v %v", e, err)
	}
	if e, err := Parse(`{"edits":[]}`); err != nil || len(e) != 0 {
		t.Fatalf("empty: %v %v", e, err)
	}
	if _, err := Parse("no json"); !errors.Is(err, ErrNoAnswer) {
		t.Fatal(err)
	}
}

// model answers each window from a table keyed by a substring of its text.
type model struct {
	mu      sync.Mutex
	replies map[string]string
	seen    []string
	refuse  string
}

func (m *model) call(_ context.Context, system, user string) (string, error) {
	text := user[strings.Index(user, "TEXT\n")+5 : strings.Index(user, "\n\nCANDIDATES")]
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, text)
	if m.refuse != "" && strings.Contains(text, m.refuse) {
		return "", fmt.Errorf("%w: rate limited", ErrRefused)
	}
	for k, v := range m.replies {
		if strings.Contains(text, k) {
			return v, nil
		}
	}
	return `{"edits":[]}`, nil
}

func TestPassRewritesGuardedEditsPerWindow(t *testing.T) {
	m := &model{replies: map[string]string{
		"我跟":  `{"edits":[{"from":"小西","to":"小溪","person":"林小溪"},{"from":"天气","to":"小溪","person":"林小溪"}]}`,
		"来了吗": `{"edits":[{"from":"明元","to":"明远","person":"周明远"},{"from":"小西","to":"小溪","person":"林小溪"}]}`,
		"说好":  `{"edits":[{"from":"明元","to":"明远","person":"周明远"}]}`,
	}}
	p := Pass{Index: testIndex(t), Call: m.call, Concurrency: 2, WindowChars: 12}
	res := p.Run(context.Background(), []Transcript{
		{Name: "ours", Texts: []string{"我跟小西说", "天气不错", "", "明元来了吗", "小西走了"}},
		{Name: "feishu", Texts: []string{"明元说好"}},
	})
	want := []string{"我跟小溪说", "天气不错", "", "明远来了吗", "小溪走了"}
	if !reflect.DeepEqual(res.Texts["ours"], want) || res.Texts["feishu"][0] != "明远说好" {
		t.Fatalf("texts %q / %q", res.Texts["ours"], res.Texts["feishu"])
	}
	if res.Windows != 3 || res.Calls != 3 || res.Failed != 0 {
		t.Fatalf("windows %d calls %d failed %d (seen %q)", res.Windows, res.Calls, res.Failed, m.seen)
	}
	if len(res.Changes) != 4 || res.Changes[0].Names[0] != (Name{Text: "小溪", Person: "lin-xiaoxi", Heard: "小西"}) || res.Changes[3].Transcript != "feishu" {
		t.Fatalf("changes %+v", res.Changes)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].Why != "from does not sound like to" {
		t.Fatalf("dropped %+v", res.Dropped)
	}
}

func TestPassStopsAtARefusal(t *testing.T) {
	m := &model{refuse: "一", replies: map[string]string{"小西": `{"edits":[{"from":"小西","to":"小溪","person":"林小溪"}]}`}}
	p := Pass{Index: testIndex(t), Call: m.call, Concurrency: 1, WindowChars: 1}
	texts := []string{"小西说", "一二", "三四", "五六"}
	res := p.Run(context.Background(), []Transcript{{Name: "ours", Texts: texts}})
	if res.Refused == "" || res.Texts["ours"][0] != "小溪说" || len(m.seen) != 2 {
		t.Fatalf("refused %q, texts %q, calls made %q", res.Refused, res.Texts["ours"], m.seen)
	}
}

func TestPassFocusOnlyCallsWindowsThatMayNameThem(t *testing.T) {
	m := &model{}
	p := Pass{Index: testIndex(t), Call: m.call, Concurrency: 1, WindowChars: 1, Focus: []int{1}}
	p.Run(context.Background(), []Transcript{{Name: "ours", Texts: []string{"小西说", "明元来了", "今天天气"}}})
	if !reflect.DeepEqual(m.seen, []string{"明元来了"}) {
		t.Fatalf("called %q", m.seen)
	}
}
