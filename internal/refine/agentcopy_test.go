package refine

import "testing"

// A "both" delivery is the model's text, a blank line and the recognised
// text under its mark, whether the model fixed words or rewrote the text;
// nothing is added when nothing but spacing changed.
func TestBoth(t *testing.T) {
	const heard = "用海库跑一下 第八哥"
	for _, tc := range []struct{ name, text, want string }{
		{"unchanged", heard, heard},
		{"spacing", " 用海库跑一下  第八哥\n", " 用海库跑一下  第八哥\n"},
		{"words", "用Haiku跑一下 debug", "用Haiku跑一下 debug\n\n" + MarkOriginal + "\n" + heard},
		{"rewrite", "跑一下 Haiku 再 debug", "跑一下 Haiku 再 debug\n\n" + MarkOriginal + "\n" + heard},
	} {
		if got := Both(heard, tc.text); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An edit is shown cut between words: the words both sides share at either
// end go, a word one side changed stays whole, and CJK characters each
// count as a word.
func TestShown(t *testing.T) {
	for _, tc := range []struct{ from, to, want string }{
		{"super adviser", "supervisor", "super adviser → supervisor"},
		{"用海库跑", "用Haiku跑", "海库 → Haiku"},
		{"Low search skill", "log search skill", "Low → log"},
		{"the name machine of lazy cat", "the NAS machine of lazy cat", "name → NAS"},
		{"Mac Max connection", "max_connections", "Mac Max connection → max_connections"},
		{"XVF two", "XVF2", "XVF two → XVF2"},
		{"跑一下", "跑一下吧", "跑一下 → 跑一下吧"},
	} {
		if f, to := Shown(tc.from, tc.to); f+" → "+to != tc.want {
			t.Errorf("Shown(%q, %q) = %q → %q, want %s", tc.from, tc.to, f, to, tc.want)
		}
	}
}
