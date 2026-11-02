package cmd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("a", 199) + "é and more"
	got := truncate(s, 200)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > 200+len("…") {
		t.Errorf("truncate = %q", got)
	}
	if truncate("short", 200) != "short" {
		t.Error("short strings stay as they are")
	}
}
