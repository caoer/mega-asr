package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caoer/mega-asr/internal/score"
)

// score judges hypothesis files against an eval manifest.
//
//	asrbench score -manifest M.jsonl [-terms FILE] [-rows OUT.tsv] HYP.jsonl...
//
// Manifest rows: {id, wav, ref, lang: zh|en|mixed, dur_s, source}. Hypothesis
// rows: {id, text, latency_ms}; the tag is the file name without .jsonl. Only
// ids present in a hypothesis file are scored; the clips column shows the
// coverage (k/N) per bucket, so a model that skips clips is visible.
//
// Normalisation and alignment: package internal/score.
//
// Metrics: the token error rate (substitutions + deletions + insertions over
// reference tokens, summed over the bucket's clips) is CER for zh (tokens are
// characters), WER for en (words) and MER for mixed (characters + words).
// Punctuation is scored separately on the aligned token pairs (match or
// substitution): the mark after each token is none, pause (, 、 ; :) or end
// (. 。 ? ! …); punct_f1 counts a hit when both sides carry the same class,
// end_f1 looks at end marks only. Named terms (-terms, one per line, # comments;
// default megavoice's hotwords.txt) are matched as token sequences: per clip a
// term found n_ref times in the reference and n_hyp times in the hypothesis
// scores min(n_ref, n_hyp) hits of n_ref; term_hit is hits / n_ref. han_er and
// lat_er split the errors by script: substitutions and deletions count against
// the reference token's script, insertions against the hypothesis token's,
// each over the reference's tokens of that script. -confusions N lists the
// most frequent error spans (a run of non-matching alignment steps, reference
// side → hypothesis side; ∅ is empty).
func scoreCmd(args []string) error {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "eval manifest (jsonl)")
	termsPath := fs.String("terms", hotwordsPath(), "named terms, one per line")
	rowsOut := fs.String("rows", "", "write per-clip scores to this TSV")
	nConf := fs.Int("confusions", 0, "print the N most frequent error spans (reference → hypothesis) per file")
	if err := fs.Parse(args); err != nil || *manifest == "" || fs.NArg() == 0 {
		return fmt.Errorf("score -manifest M.jsonl [-terms FILE] [-rows OUT.tsv] [-confusions N] HYP.jsonl...")
	}
	man, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	terms, err := readTerms(*termsPath)
	if err != nil {
		return err
	}
	var rows *bufio.Writer
	if *rowsOut != "" {
		f, err := os.Create(*rowsOut)
		if err != nil {
			return err
		}
		defer f.Close()
		rows = bufio.NewWriter(f)
		defer rows.Flush()
		fmt.Fprintln(rows, "tag\tid\tlang\tsource\tdur_s\tref_tokens\terrors\trate\tsub\tdel\tins\tterm_ref\tterm_hit\tlatency_ms\than_ref\than_err\tlat_ref\tlat_err")
	}
	fmt.Printf("# %d manifest clips, %d terms (%s)\n", len(man), len(terms), *termsPath)
	fmt.Println("tag\tbucket\tsource\tclips\tmetric\tref_tokens\terrors\trate\tsub\tdel\tins\tpunct_f1\tend_f1\tterm_hit\tp50_ms\tp90_ms\than_er\tlat_er")
	for _, hp := range fs.Args() {
		hyps, err := readHyps(hp)
		if err != nil {
			return err
		}
		tag := strings.TrimSuffix(filepath.Base(hp), ".jsonl")
		groups := map[[2]string]*score.Tally{}
		total := map[[2]string]int{}
		add := func(k [2]string) *score.Tally {
			if groups[k] == nil {
				groups[k] = &score.Tally{}
			}
			return groups[k]
		}
		sources := map[string]bool{}
		conf := map[[2]string]int{}
		for _, m := range man {
			sources[m.Source] = true
			keys := [][2]string{{m.Lang, "all"}, {"all", "all"}, {m.Lang, m.Source}}
			for _, k := range keys {
				total[k]++
			}
			h, ok := hyps[m.ID]
			if !ok {
				continue
			}
			c := score.ScoreClip(m.Ref, h.Text, terms)
			for _, k := range keys {
				add(k).Add(c, h.LatencyMS)
			}
			for _, cf := range c.Confusions {
				conf[cf]++
			}
			if rows != nil {
				fmt.Fprintf(rows, "%s\t%s\t%s\t%s\t%.2f\t%d\t%d\t%.4f\t%d\t%d\t%d\t%d\t%d\t%.0f\t%d\t%d\t%d\t%d\n", tag, m.ID, m.Lang, m.Source, m.DurS,
					c.RefTok, c.Errors(), c.Rate(), c.Sub, c.Del, c.Ins, c.TermRef, c.TermHit, h.LatencyMS, c.HanRef, c.HanErr, c.LatRef, c.LatErr)
			}
		}
		var keys [][2]string
		for _, b := range []string{"zh", "en", "mixed", "all"} {
			keys = append(keys, [2]string{b, "all"})
			if len(sources) > 1 && b != "all" {
				var ss []string
				for s := range sources {
					ss = append(ss, s)
				}
				sort.Strings(ss)
				for _, s := range ss {
					keys = append(keys, [2]string{b, s})
				}
			}
		}
		for _, k := range keys {
			t := groups[k]
			if total[k] == 0 {
				continue
			}
			if t == nil {
				t = &score.Tally{}
			}
			fmt.Printf("%s\t%s\t%s\t%d/%d\t%s\t%d\t%d\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", tag, k[0], k[1], t.Clips, total[k], metricName(k[0]),
				t.RefTok, t.Errors(), pct(t.Errors(), t.RefTok), t.Sub, t.Del, t.Ins,
				f1(t.PTP, t.PFP, t.PFN), f1(t.ETP, t.EFP, t.EFN), frac(t.TermHit, t.TermRef), ms(t.Lat, 0.5), ms(t.Lat, 0.9),
				pct(t.HanErr, t.HanRef), pct(t.LatErr, t.LatRef))
		}
		if *nConf > 0 {
			type kv struct {
				k [2]string
				n int
			}
			var all []kv
			for k, n := range conf {
				all = append(all, kv{k, n})
			}
			sort.Slice(all, func(i, j int) bool {
				if all[i].n != all[j].n {
					return all[i].n > all[j].n
				}
				return all[i].k[0]+all[i].k[1] < all[j].k[0]+all[j].k[1]
			})
			for _, e := range all[:min(*nConf, len(all))] {
				fmt.Printf("confusion\t%s\t%d\t%s\t→\t%s\n", tag, e.n, or(e.k[0], "∅"), or(e.k[1], "∅"))
			}
		}
	}
	return nil
}

func metricName(bucket string) string {
	switch bucket {
	case "zh":
		return "CER"
	case "en":
		return "WER"
	case "mixed":
		return "MER"
	}
	return "TER"
}

type manifestRow struct {
	ID     string  `json:"id"`
	Wav    string  `json:"wav"`
	Ref    string  `json:"ref"`
	Lang   string  `json:"lang"`
	DurS   float64 `json:"dur_s"`
	Source string  `json:"source"`
}

type hypRow struct {
	ID        string  `json:"id"`
	Text      string  `json:"text"`
	LatencyMS float64 `json:"latency_ms"`
}

func readJSONL(path string, each func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		if err := each(sc.Bytes()); err != nil {
			return fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	return sc.Err()
}

// readManifest keeps the rows that have a reference, in file order.
func readManifest(path string) ([]manifestRow, error) {
	var man []manifestRow
	seen := map[string]bool{}
	err := readJSONL(path, func(b []byte) error {
		var m manifestRow
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if seen[m.ID] {
			return fmt.Errorf("duplicate id %q", m.ID)
		}
		seen[m.ID] = true
		if strings.TrimSpace(m.Ref) != "" {
			man = append(man, m)
		}
		return nil
	})
	return man, err
}

func readHyps(path string) (map[string]hypRow, error) {
	hyps := map[string]hypRow{}
	err := readJSONL(path, func(b []byte) error {
		var h hypRow
		if err := json.Unmarshal(b, &h); err != nil {
			return err
		}
		hyps[h.ID] = h // a later row for the same id wins (a re-run appended)
		return nil
	})
	return hyps, err
}

// readTerms reads one term per line; a missing file is no terms.
func readTerms(path string) ([][]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var terms [][]string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if t := score.Words(score.Tokenize(l)); len(t) > 0 {
			terms = append(terms, t)
		}
	}
	return terms, nil
}

func pct(num, den int) string {
	if den == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", 100*float64(num)/float64(den))
}

func frac(num, den int) string {
	if den == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", num, den)
}

func f1(tp, fp, fn int) string {
	if tp+fp+fn == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", 2*float64(tp)/float64(2*tp+fp+fn))
}

func ms(lat []float64, p float64) string {
	if len(lat) == 0 {
		return "-"
	}
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	return fmt.Sprintf("%.0f", percentile(s, p))
}
