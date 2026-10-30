package registry

import "testing"

func TestCompareBarePreReleaseSuffix(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3-rc", "1.2.3-rc1", -1},
		{"1.2.3-rc1", "1.2.3-rc", 1},
		{"1.2.3-beta", "1.2.3-beta2", -1},
		{"1.2.3-beta", "1.2.3-beta.1", -1},
		{"1.2.3-rc", "1.2.3-rc", 0},
		{"1.2.3-rc", "1.2.3", -1},
	}
	for _, c := range cases {
		ta, ok := ParseTag(c.a)
		if !ok {
			t.Fatalf("ParseTag(%q) failed", c.a)
		}
		tb, ok := ParseTag(c.b)
		if !ok {
			t.Fatalf("ParseTag(%q) failed", c.b)
		}
		if got := Compare(ta, tb); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPickHighestNeverPicksBareSuffixDowngrade(t *testing.T) {
	cur, _ := ParseTag("1.2.3-rc1")
	tags := []string{"1.2.3-rc1", "1.2.3-rc2", "1.2.3-rc"}
	if got := PickHighest(cur, tags, "patch"); got != "1.2.3-rc2" {
		t.Errorf("PickHighest = %q, want 1.2.3-rc2", got)
	}
	curBare, _ := ParseTag("1.2.3-rc")
	if got := PickHighest(curBare, tags, "patch"); got != "1.2.3-rc2" {
		t.Errorf("PickHighest from bare rc = %q, want 1.2.3-rc2", got)
	}
}
