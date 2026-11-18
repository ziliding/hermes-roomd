package roster

import "testing"

func TestGroupMatcher(t *testing.T) {
	g := NewGroupMatcher(DefaultGroupWords)
	tests := []struct {
		body string
		want bool
	}{
		{"你们觉得呢?", true},
		{"大家晚上好", true},
		{"What does everyone think?", true},
		{"what do you all think", true},
		{"EVERYBODY, listen", true},
		{"all good, thanks", false},
		{"Noir, what do you think?", false},
		{"everyoneelse", false},
	}
	for _, tt := range tests {
		if got := g.Match(tt.body); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.body, got, tt.want)
		}
	}
	if NewGroupMatcher(nil).Match("你们") {
		t.Error("no words should match nothing")
	}
}
