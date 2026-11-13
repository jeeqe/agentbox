package runner

import (
	"strings"
	"testing"
)

func TestCleanLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"colour reset", "\x1b[32mok\x1b[0m done", "ok done"},
		{"bare reset", "text\x1b[0m", "text"},
		{"256 colour and bold", "\x1b[1;38;5;208mwarn\x1b[22;39m", "warn"},
		{"cursor and erase", "\x1b[2K\x1b[1Gbuilding", "building"},
		{"osc title with bel", "\x1b]0;my title\x07hello", "hello"},
		{"osc hyperlink", "\x1b]8;;https://x.dev\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"progress redraw", "10%\r50%\r100% done", "100% done"},
		{"trailing carriage return", "line\r", "line"},
		{"windows line end", "line\r", "line"},
		{"control chars", "a\x00b\x07c\x7fd", "abcd"},
		{"tab kept", "a\tb", "a\tb"},
		{"plain unicode", "héllo → ✓", "héllo → ✓"},
		{"invalid utf8", "a\xffb", "a�b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanLine(c.in); got != c.want {
				t.Errorf("cleanLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestReadLinesHandlesLongLinesAndKeepsReading(t *testing.T) {
	long := strings.Repeat("x", maxLogLine*3)
	input := "first\n" + long + "\nlast"
	var got []string
	readLines(strings.NewReader(input), func(s string) { got = append(got, s) })
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	if got[0] != "first" || got[2] != "last" {
		t.Errorf("surrounding lines wrong: %q ... %q", got[0], got[2])
	}
	if !strings.HasSuffix(got[1], "…[line cut]") || len(got[1]) > maxLogLine+32 {
		t.Errorf("long line not cut cleanly (len %d)", len(got[1]))
	}
}

func TestReadLinesStripsANSIPerLine(t *testing.T) {
	var got []string
	readLines(strings.NewReader("\x1b[31mred\x1b[0m\n\x1b[0m\n"), func(s string) { got = append(got, s) })
	if len(got) != 2 || got[0] != "red" || got[1] != "" {
		t.Fatalf("got %q", got)
	}
}
