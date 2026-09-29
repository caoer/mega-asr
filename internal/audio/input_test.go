package audio

import (
	"encoding/json"
	"testing"
)

// Every capture source names its device for the take's input record.
func TestTakeInputPerSource(t *testing.T) {
	for _, c := range []struct {
		src  Described
		want string
	}{
		{&XVF{Host: "micbox", Device: "hw:Array,0", Channel: 2}, `{"source":"ssh","host":"micbox","pcm":"hw:Array,0","channel":2}`},
		{&XVF{Device: "default"}, `{"source":"local","pcm":"default","channel":0}`},
		{&Remote{Name: "micbox", Addr: "192.0.2.140:7866", Token: "secret"}, `{"source":"remote","name":"micbox","host":"192.0.2.140:7866","channel":0}`},
		{&File{Path: "/u/20200311-140522.wav"}, `{"source":"file","channel":0,"file":"/u/20200311-140522.wav"}`},
	} {
		b, err := json.Marshal(c.src.Input())
		if err != nil || string(b) != c.want {
			t.Errorf("%T: %s, %v; want %s", c.src, b, err, c.want)
		}
	}
}
