package proc

import "testing"

func TestClampWaitSeconds(t *testing.T) {
	cases := []struct {
		requested int
		want      int
	}{
		{requested: 0, want: 1},
		{requested: -5, want: 1},
		{requested: 1, want: 1},
		{requested: 30, want: 30},
		{requested: MaxWaitSeconds, want: MaxWaitSeconds},
		{requested: MaxWaitSeconds + 1, want: MaxWaitSeconds},
		{requested: 999999, want: MaxWaitSeconds},
	}
	for _, c := range cases {
		got := clampWaitSeconds(c.requested)
		if got != c.want {
			t.Errorf("clampWaitSeconds(%d) = %d, want %d", c.requested, got, c.want)
		}
	}
}

func TestTrailingLines(t *testing.T) {
	cases := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{name: "empty input", input: "", n: 5, want: ""},
		{name: "zero lines", input: "hello\n", n: 0, want: ""},
		{name: "negative lines", input: "hello\n", n: -1, want: ""},
		{name: "single line with newline", input: "hello\n", n: 1, want: "hello\n"},
		{name: "single line with newline requesting more", input: "hello\n", n: 5, want: "hello\n"},
		{name: "single line without newline", input: "hello", n: 1, want: "hello"},
		{name: "single line without newline requesting more", input: "hello", n: 5, want: "hello"},
		{name: "exact lines", input: "1\n2\n3\n", n: 3, want: "1\n2\n3\n"},
		{name: "fewer lines than requested", input: "1\n2\n3\n", n: 10, want: "1\n2\n3\n"},
		{name: "last 1 line of 5", input: "1\n2\n3\n4\n5\n", n: 1, want: "5\n"},
		{name: "last 2 lines of 5", input: "1\n2\n3\n4\n5\n", n: 2, want: "4\n5\n"},
		{name: "last 3 lines of 5", input: "1\n2\n3\n4\n5\n", n: 3, want: "3\n4\n5\n"},
		{name: "last 1 of partial ending", input: "1\n2\n3\npartial", n: 1, want: "partial"},
		{name: "last 2 of partial ending", input: "1\n2\n3\npartial", n: 2, want: "3\npartial"},
		{name: "last 3 of partial ending", input: "1\n2\n3\npartial", n: 3, want: "2\n3\npartial"},
		{name: "all lines of partial ending", input: "1\n2\n3\npartial", n: 4, want: "1\n2\n3\npartial"},
		{name: "more lines than partial ending", input: "1\n2\n3\npartial", n: 10, want: "1\n2\n3\npartial"},
		{name: "single newline", input: "\n", n: 1, want: "\n"},
		{name: "single newline requesting more", input: "\n", n: 5, want: "\n"},
		{name: "two newlines last 1", input: "\n\n", n: 1, want: "\n"},
		{name: "two newlines last 2", input: "\n\n", n: 2, want: "\n\n"},
		{name: "blank lines between content", input: "a\n\nb\n", n: 2, want: "\nb\n"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(trailingLines([]byte(c.input), c.n))
			if got != c.want {
				t.Errorf("trailingLines(%q, %d) = %q, want %q", c.input, c.n, got, c.want)
			}
		})
	}
}
