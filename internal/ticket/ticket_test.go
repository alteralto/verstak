package ticket

import (
	"testing"
	"time"
)

var at = time.Date(2026, 10, 21, 1, 30, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	tk, err := Parse("\n\n# Ракеты не убираются из руки\r\n\nПосле посадки ракеты остаются в руке.\nВторая строка.\n", at)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Title != "Ракеты не убираются из руки" {
		t.Errorf("title %q", tk.Title)
	}
	if tk.Body != "После посадки ракеты остаются в руке.\nВторая строка." {
		t.Errorf("body %q", tk.Body)
	}
	if tk.ID != "20261021-013000-rakety-ne-ubirayutsya-iz-ruki" {
		t.Errorf("id %q", tk.ID)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, s := range []string{"", "  \n\n", "#\n##  \n"} {
		if _, err := Parse(s, at); err == nil {
			t.Errorf("%q: want an error", s)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Fix goTo: return bool!": "fix-goto-return-bool",
		"  ---  ":                "task",
		"😀 emoji only":           "emoji-only",
		"Щука ёж":                "schuka-ezh",
		"a very long title that goes on and on and on": "a-very-long-title-that-goes-on-a",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
