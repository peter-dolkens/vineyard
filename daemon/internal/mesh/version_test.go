package mesh

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"v0.3.10", "0.3.9", true},
		{"0.3.10", "v0.3.9", true},
		{"v0.3.9", "v0.3.10", false},
		{"v0.3.10", "v0.3.10", false},
		{"v0.3.10-dev", "v0.3.10-3-gabc", false},
		{"1.0.0", "0.9.99", true},
		{"dev", "0.3.9", false},
		{"0.3.10", "dev", false},
		{"", "0.3.9", false},
		{"test", "test", false},
	}
	for _, c := range cases {
		if got := isNewer(c.cand, c.cur); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.cand, c.cur, got, c.want)
		}
	}
}
