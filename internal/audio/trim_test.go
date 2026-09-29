package audio

import "testing"

func TestTrim(t *testing.T) {
	s := make([]int16, 3*Rate) // 1 s quiet, 1 s loud, 1 s quiet
	for i := Rate; i < 2*Rate; i++ {
		s[i] = int16(8000 * (i%32 - 16) / 16)
	}
	for i := range s {
		s[i] += int16(i%7 - 3) // a noise floor, so quiet blocks have a level
	}
	got := Trim(s)
	if want := Rate + 2*Rate/5; len(got) != want || &got[0] != &s[Rate-Rate/5] {
		t.Fatalf("kept %d samples, want %d starting 200 ms before the speech", len(got), want)
	}
	quiet := make([]int16, Rate)
	if len(Trim(quiet)) != Rate {
		t.Fatal("a clip with no loud block must come back whole")
	}
}
