package notify

import "strings"

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func slackEscape(s string) string { return slackEscaper.Replace(s) }

func fenceSafe(s string) string {
	if !strings.Contains(s, "``") {
		return s
	}
	var b strings.Builder
	prev := rune(0)
	for _, r := range s {
		if r == '`' && prev == '`' {
			b.WriteRune('\u200b')
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

var discordEscaper = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`,
	">", `\>`, "<", `\<`, "[", `\[`, "]", `\]`, "#", `\#`, "@", `\@`,
)

func discordEscape(s string) string { return discordEscaper.Replace(s) }

func discordEscapeWithin(s string, n int) string {
	if e := discordEscape(s); len(e) <= n {
		return e
	}
	const ellipsis = "…"
	var b strings.Builder
	for _, r := range s {
		e := discordEscape(string(r))
		if b.Len()+len(e)+len(ellipsis) > n {
			break
		}
		b.WriteString(e)
	}
	return b.String() + ellipsis
}

func code(s string) string { return codeWithin(s, -1) }

func codeWithin(s string, n int) string {
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "`"):
		if n < 0 {
			return discordEscape(s)
		}
		return discordEscapeWithin(s, n)
	case n < 0:
		return "`" + s + "`"
	}
	return "`" + truncate(s, n-2) + "`"
}

func teamsText(s string) string { return strings.ReplaceAll(s, "](", "]\u200b(") }
