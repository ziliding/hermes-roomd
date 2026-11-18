package silence

import "testing"

func TestIs(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true}, {"  ", true}, {"[SILENT]", true}, {"silent", true}, {"SILENT.", true},
		{"NO_REPLY", true}, {"no reply", true}, {"NO REPLY!", true}, {"(silent)", true},
		{"*stays silent*", true}, {"(remains silent)", true}, {"🔇", true}, {".", true},
		{"...", true}, {"…", true}, {". . .", true},
		{"👍", false}, {"😂", false}, {"♪", false}, {"?", false}, {"!", false}, {"—", false},
		{"ok.", false}, {"pass", false}, {"I'll stay silent on that.", false}, {"❤️", false},
	}
	for _, tt := range tests {
		if got := Is(tt.in); got != tt.want {
			t.Errorf("Is(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
