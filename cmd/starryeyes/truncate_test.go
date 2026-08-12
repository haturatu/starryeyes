package main

import "testing"

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		input string
		limit int
		want  string
	}{
		{"hello", -1, ""},
		{"hello", 0, ""},
		{"hello", 3, "hel"},
		{"hello", 5, "hello"},
		{"hello", 8, "hello"},
	} {
		if got := truncate(tc.input, tc.limit); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.limit, got, tc.want)
		}
	}
}

func FuzzTruncate(f *testing.F) {
	f.Add("0", -1)
	f.Add("hello", 3)
	f.Fuzz(func(t *testing.T, input string, limit int) {
		got := truncate(input, limit)
		if limit <= 0 {
			if got != "" {
				t.Fatalf("truncate(%q, %d) = %q, want empty string", input, limit, got)
			}
			return
		}
		if len(got) > limit || got != input[:len(got)] {
			t.Fatalf("truncate(%q, %d) = %q", input, limit, got)
		}
	})
}
