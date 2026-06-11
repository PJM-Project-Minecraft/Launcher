package launcherrelease

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.1.0", 1},
		{"0.10.0", "0.9.0", 1},   // числовое, не лексикографическое
		{"1.2", "1.2.0", 0},      // отсутствующий сегмент = 0
		{"1.2.1", "1.2", 1},
		{"abc", "0.0.1", -1},     // мусор = 0.0.0
		{"", "0.0.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValidVersion(t *testing.T) {
	valid := []string{"0.1.0", "1.0.0", "10.20.30"}
	invalid := []string{"", "1.0", "1.0.0.0", "v1.0.0", "1.0.x", "1..0", " 1.0.0"}
	for _, v := range valid {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true, want false", v)
		}
	}
}
