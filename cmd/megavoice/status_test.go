//go:build darwin

package main

import (
	"encoding/json"
	"testing"

	"github.com/caoer/mega-asr/internal/mac"
)

func TestStatusCarriesMicrophone(t *testing.T) {
	b, err := json.Marshal(status{Grants: mac.Grants{Microphone: true}})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Grants map[string]any `json:"grants"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Grants["microphone"] != true {
		t.Fatalf("status %s: no grants.microphone", b)
	}
}
