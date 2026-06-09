package web

import "testing"

// csvSafe defends against CSV/formula injection: a cell that begins with a
// spreadsheet formula trigger gets a leading quote so Excel/Sheets won't execute
// it. Everything else (and the trigger anywhere but position 0) is untouched.
func TestCsvSafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=1+1", "'=1+1"},
		{"+1", "'+1"},
		{"-1", "'-1"},
		{"@cmd", "'@cmd"},
		{"\tx", "'\tx"},
		{"\rx", "'\rx"},
		{"=HYPERLINK(\"http://evil\")", "'=HYPERLINK(\"http://evil\")"},
		{"hello", "hello"},
		{"", ""},
		{"1.2.3.4", "1.2.3.4"},
		{"AS13335 Cloudflare", "AS13335 Cloudflare"},
		{"a=b", "a=b"}, // trigger only matters at position 0
	}
	for _, c := range cases {
		if got := csvSafe(c.in); got != c.want {
			t.Errorf("csvSafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
