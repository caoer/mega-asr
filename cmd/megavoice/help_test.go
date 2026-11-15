package main

import "testing"

func TestHelpAsked(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"revoke", "--help"}, true},
		{[]string{"--", "--help"}, false},
		{[]string{"--helper"}, false},
		{[]string{"rotate"}, false},
	} {
		if got := helpAsked(tc.args); got != tc.want {
			t.Errorf("helpAsked(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
