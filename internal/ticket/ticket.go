// Package ticket reads a task for the coding agent: a title line and free text, as a player or a
// developer would write it ("ракеты не убираются из руки после полёта").
package ticket

import (
	"errors"
	"strings"
	"time"
	"unicode"
)

// Ticket is one task. ID names its workspace and branch.
type Ticket struct {
	ID    string
	Title string
	Body  string
}

// Parse takes the first non-empty line as the title (a leading "#" is dropped) and the rest as
// the body.
func Parse(text string, now time.Time) (Ticket, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, l := range lines {
		title := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#"))
		if title == "" {
			continue
		}
		return Ticket{
			ID:    now.Format("20060102-150405") + "-" + Slug(title),
			Title: title,
			Body:  strings.TrimSpace(strings.Join(lines[i+1:], "\n")),
		}, nil
	}
	return Ticket{}, errors.New("empty ticket")
}

// Markdown is the ticket as the agent reads it.
func (t Ticket) Markdown() string {
	if t.Body == "" {
		return "# " + t.Title + "\n"
	}
	return "# " + t.Title + "\n\n" + t.Body + "\n"
}

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z",
	'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// slugMax keeps branch and folder names short.
const slugMax = 32

// Slug is the title in latin letters, digits and dashes, for a folder and a branch name.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		s, ok := translit[r]
		switch {
		case ok:
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			s = string(r)
		default:
			s = "-"
		}
		if s == "-" {
			if !dash && b.Len() > 0 {
				b.WriteString(s)
			}
			dash = true
			continue
		}
		if b.Len()+len(s) > slugMax {
			break
		}
		b.WriteString(s)
		dash = false
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}
