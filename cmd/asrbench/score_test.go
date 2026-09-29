package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScoreCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	man := write("manifest.jsonl", `{"id":"a","ref":"面包烤焦了，再来一炉。","lang":"zh","source":"megavoice","wav":"/x/a.wav","dur_s":2}
{"id":"b","ref":"Is the oven hot yet?","lang":"en","source":"megavoice","wav":"/x/b.wav","dur_s":3}
{"id":"c","ref":"先把 oven 里的 timer 关掉。","lang":"mixed","source":"megavoice","wav":"/x/c.wav","dur_s":4}
`)
	hyp := write("nano.jsonl", `{"id":"a","text":"面包烤焦了再来一路。","latency_ms":700}
{"id":"c","text":"先把oven里的time关。","latency_ms":900}
`)
	terms := write("terms.txt", "# test\noven\ntimer\n")
	out, err := exec.Command("go", "run", ".", "score", "-manifest", man, "-terms", terms, "-rows", filepath.Join(dir, "rows.tsv"), hyp).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{
		"nano\tzh\tall\t1/1\tCER\t9\t1\t11.11%",
		"nano\ten\tall\t0/1\tWER\t0\t0\t-",
		"nano\tmixed\tall\t1/1\tMER\t8\t2\t25.00%",
		"nano\tall\tall\t2/3\tTER\t17\t3\t17.65%",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(string(out), "nano\tmixed\tall\t1/1\tMER\t8\t2\t25.00%") {
		t.Errorf("no mixed row")
	}
	rows, _ := os.ReadFile(filepath.Join(dir, "rows.tsv"))
	if n := strings.Count(string(rows), "\n"); n != 3 {
		t.Errorf("rows.tsv has %d lines, want header + 2", n)
	}
}
