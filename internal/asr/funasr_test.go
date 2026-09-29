package asr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// the shape of llama-funasr-cli --vad stdout
		{"黑麦粉吸水多，水量要比白面粉多加一成。\n", "黑麦粉吸水多，水量要比白面粉多加一成。"},
		{"\n", ""},     // silence
		{"/sil\n", ""}, // VAD found nothing to decode
		// segments joined: Latin with a space, Chinese without
		{"Step one done\n/sil\n/sil\nstep two done\n", "Step one done step two done"},
		{"面团发到两倍大/sil就可以整形了\n", "面团发到两倍大就可以整形了"},
		// a line break inside a number's phrase keeps the Latin spacing
		{"烤箱预热到 220\n度，二十分钟出炉\n", "烤箱预热到 220 度，二十分钟出炉"},
		{"[laugh_soft]揉面[breath]十分钟 then rest[noise]\n", "揉面十分钟 then rest"},
		{"[noise]\n", ""},
	} {
		if got := Clean(c.in); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHotwordArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hotwords.txt")
	list := "levain\n# a note\n\tbanneton \nproof, bake\n\n"
	for i := 0; i < MaxHotwords; i++ {
		list += "t\n"
	}
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	hw := ReadHotwords(path)
	if len(hw) != MaxHotwords || hw[0] != "levain" || hw[1] != "banneton" {
		t.Fatalf("ReadHotwords: %d terms, first %q", len(hw), hw[:2])
	}
	cmd := FunASR{Root: "/m", Hotwords: func() []string { return []string{"levain", "banneton"} }, EncGPU: true, VADGPU: true}.command(context.Background(), "a.wav", false)
	got := strings.Join(cmd.Args[1:], " ")
	if !strings.Contains(got, "--hotwords levain, banneton --hotwords-cjk --enc-gpu --vad-gpu -a a.wav") {
		t.Errorf("args %q", got)
	}
	plain := FunASR{Root: "/m", Hotwords: func() []string { return nil }}.command(context.Background(), "a.wav", false)
	if strings.Contains(strings.Join(plain.Args, " "), "--hotwords") {
		t.Errorf("empty list still passes --hotwords: %q", plain.Args)
	}
	if got := strings.Join(plain.Args, " "); !strings.Contains(got, "-m /m/models/qwen3-0.6b-q8_0.gguf") {
		t.Errorf("no llm: want root's Qwen3, got %q", got)
	}
	if g := strings.Join(FunASR{Root: "/m", GPU: 99}.command(context.Background(), "a.wav", false).Args, " "); !strings.Contains(g, "--gpu 99") {
		t.Errorf("GPU 99 args %q", g)
	}
	ft := FunASR{Root: "/m", LLM: "/ft/tuned.gguf"}.command(context.Background(), "a.wav", false)
	if got := strings.Join(ft.Args, " "); !strings.Contains(got, "-m /ft/tuned.gguf") || !strings.Contains(got, "--enc /m/models/funasr-encoder-f16.gguf") {
		t.Errorf("llm override: %q", got)
	}
	whole := FunASR{Root: "/m", VADGPU: true}.command(context.Background(), "a.wav", true)
	if got := strings.Join(whole.Args, " "); strings.Contains(got, "--vad") {
		t.Errorf("a whole request still runs the VAD: %q", got)
	}
}
