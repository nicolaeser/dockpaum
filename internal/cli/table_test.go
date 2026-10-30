package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"":               0,
		"web":            3,
		"größe":          5,
		"e\u0301":        1,
		"日本":             4,
		"🚀 ok":           5,
		"a\u200bb":       2,
		"ｆｕｌｌ":           8,
		"->":             2,
		"Sun 03:30 CEST": 14,
	}
	for s, want := range cases {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestWriteTableAlignsWithAndWithoutColor(t *testing.T) {
	header := []string{"NAME", "PLAN", "REASON"}
	rows := [][]string{{"web", "update", "newer"}, {"日本", "wait", "soak"}, {"db", "none", "-"}}
	style := func(row, col int) string {
		if row >= 0 && col == 1 {
			return styleGreen
		}
		return ""
	}
	var plain, colored bytes.Buffer
	if err := writeTable(&plain, header, rows, false, style); err != nil {
		t.Fatal(err)
	}
	if err := writeTable(&colored, header, rows, true, style); err != nil {
		t.Fatal(err)
	}
	want := "NAME  PLAN    REASON\nweb   update  newer\n日本  wait    soak\ndb    none    -\n"
	if plain.String() != want {
		t.Errorf("plain table:\n%q\nwant:\n%q", plain.String(), want)
	}
	if strings.Contains(plain.String(), "\x1b") {
		t.Error("plain table carries escape codes")
	}
	stripped := strings.NewReplacer(styleGreen, "", styleReset, "").Replace(colored.String())
	if stripped != want || !strings.Contains(colored.String(), styleGreen+"update"+styleReset+"  ") {
		t.Errorf("colored table:\n%q", colored.String())
	}
}
