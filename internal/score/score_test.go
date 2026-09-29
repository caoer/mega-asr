package score

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"烤 40 分钟后 check 一下颜色。", []string{"烤", "40", "分", "钟", "后", "check", "一", "下", "颜", "色"}},
		{"Ｏｖｅｎ　ｍｏｄｅ　３", []string{"oven", "mode", "3"}},
		{"Cool to 4.5°C, don’t stir; it's a low-heat step2.", []string{"cool", "to", "4.5", "c", "don't", "stir", "it's", "a", "low", "heat", "step", "2"}},
	} {
		if got := Words(Tokenize(tc.in)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	toks := Tokenize("Mix, 等., 再说!")
	var marks []byte
	for _, tk := range toks {
		marks = append(marks, tk.Punct)
	}
	if want := []byte{PunctPause, PunctEnd, 0, PunctEnd}; !reflect.DeepEqual(marks, want) {
		t.Errorf("punct classes %q, want %q", marks, want)
	}
}

// One known pair per bucket, counted by hand.
func TestScoreClipBuckets(t *testing.T) {
	terms := [][]string{{"oven"}, {"timer"}}
	for _, tc := range []struct {
		name, ref, hyp          string
		refTok, sub, del, ins   int
		termRef, termHit        int
		pTP, pFP, pFN, eTP, eFN int
	}{
		// CER: 路 for 炉, the comma missing.
		{"zh", "面包烤焦了，再来一炉。", "面包烤焦了再来一路。", 9, 1, 0, 0, 0, 0, 1, 0, 1, 1, 0},
		// WER: "not" inserted, the question mark missing.
		{"en", "Is the oven hot yet?", "is the oven not hot yet", 5, 0, 0, 1, 1, 1, 0, 0, 1, 0, 1},
		// MER: timer→time, 掉 dropped, a full stop where there was none; oven survives.
		{"mixed", "先把 oven 里的 timer 关掉。", "先把oven里的time关。", 8, 1, 1, 0, 2, 1, 0, 1, 0, 0, 0},
		// Fillers on either side cost nothing, and take their marks with them.
		{"fillers", "嗯，take it out now.", "Take it out, erm, now", 4, 0, 0, 0, 0, 0, 0, 1, 1, 0, 1},
		// Homophone characters fold.
		{"fold", "她跑得最快，它的最稳。", "他跑的最快，他地最稳。", 9, 0, 0, 0, 0, 0, 2, 0, 0, 1, 0},
		// Spelled letters join; a run of only a/i stays separate words.
		{"letters", "Route A B C first. Was I a help", "route abc first was i a help", 7, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1},
		// English number words equal digits.
		{"numbers", "Bake it forty minutes", "bake it 40 minutes", 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		c := ScoreClip(tc.ref, tc.hyp, terms)
		if c.RefTok != tc.refTok || c.Sub != tc.sub || c.Del != tc.del || c.Ins != tc.ins {
			t.Errorf("%s: ref=%d sub=%d del=%d ins=%d, want %d %d %d %d", tc.name, c.RefTok, c.Sub, c.Del, c.Ins, tc.refTok, tc.sub, tc.del, tc.ins)
		}
		if c.TermRef != tc.termRef || c.TermHit != tc.termHit {
			t.Errorf("%s: terms %d/%d, want %d/%d", tc.name, c.TermHit, c.TermRef, tc.termHit, tc.termRef)
		}
		if c.PTP != tc.pTP || c.PFP != tc.pFP || c.PFN != tc.pFN || c.ETP != tc.eTP || c.EFN != tc.eFN {
			t.Errorf("%s: punct tp=%d fp=%d fn=%d end tp=%d fn=%d, want %d %d %d %d %d", tc.name, c.PTP, c.PFP, c.PFN, c.ETP, c.EFN, tc.pTP, tc.pFP, tc.pFN, tc.eTP, tc.eFN)
		}
	}
}

func TestScoreClipScriptsAndConfusions(t *testing.T) {
	c := ScoreClip("先把 oven 里的 timer 关掉。", "先把oven里的time关。", nil)
	// timer→time is a Latin-token substitution, 掉 a CJK deletion.
	if c.HanRef != 6 || c.LatRef != 2 || c.LatErr != 1 || c.HanErr != 1 {
		t.Errorf("han %d/%d lat %d/%d, want 1/6 1/2", c.HanErr, c.HanRef, c.LatErr, c.LatRef)
	}
	want := [][2]string{{"timer", "time"}, {"掉", ""}}
	if !reflect.DeepEqual(c.Confusions, want) {
		t.Errorf("confusions %q, want %q", c.Confusions, want)
	}
}

// The script is DIR/script.tsv; a line without stamp, tab and sentence is
// skipped, and a script with no clip or no script at all is an error.
func TestScriptClips(t *testing.T) {
	dir := t.TempDir()
	if _, err := ScriptClips(dir); err == nil {
		t.Error("no script.tsv: no error")
	}
	script := "\n20200302-141500\t冰箱第二层放鸡蛋。\r\nno tab on this line\n20200302-141522\tSix jars, one lid each.\n"
	if err := os.WriteFile(filepath.Join(dir, ScriptFile), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ScriptClips(dir)
	want := []ScriptClip{{"20200302-141500", "冰箱第二层放鸡蛋。"}, {"20200302-141522", "Six jars, one lid each."}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ScriptClips = %q, %v; want %q", got, err, want)
	}
	if err := os.WriteFile(filepath.Join(dir, ScriptFile), []byte("\n\n# nothing to read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScriptClips(dir); err == nil {
		t.Error("a script with no clip: no error")
	}
}
