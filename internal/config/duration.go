package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalYAML() (interface{}, error) {
	return formatDuration(time.Duration(d)), nil
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!int" {
			n, err := strconv.ParseInt(value.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("duration: %w", err)
			}
			*d = Duration(time.Duration(n) * time.Second)
			return nil
		}
		parsed, err := ParseDuration(value.Value)
		if err != nil {
			return fmt.Errorf("duration %q: %w", value.Value, err)
		}
		*d = Duration(parsed)
		return nil
	default:
		return fmt.Errorf("duration: expected scalar, got %v", value.Kind)
	}
}

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("missing magnitude")
	}
	rewritten, err := rewriteDaysAndWeeks(s)
	if err != nil {
		return 0, err
	}
	out, err := time.ParseDuration(rewritten)
	if err != nil {
		return 0, err
	}
	if negative {
		out = -out
	}
	return out, nil
}

func rewriteDaysAndWeeks(s string) (string, error) {
	var b strings.Builder
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		j := i
		for j < len(runes) && (isDigitRune(runes[j]) || runes[j] == '.') {
			j++
		}
		if j == i {
			b.WriteRune(runes[i])
			i++
			continue
		}
		num := string(runes[i:j])
		k := j
		for k < len(runes) && isUnitRune(runes[k]) {
			k++
		}
		unit := string(runes[j:k])
		switch unit {
		case "d", "D":
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return "", fmt.Errorf("bad number %q", num)
			}
			b.WriteString(strconv.FormatFloat(f*24, 'f', -1, 64))
			b.WriteString("h")
		case "w", "W":
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return "", fmt.Errorf("bad number %q", num)
			}
			b.WriteString(strconv.FormatFloat(f*24*7, 'f', -1, 64))
			b.WriteString("h")
		case "":
			if i != 0 || k != len(runes) {
				return "", fmt.Errorf("missing unit after %q", num)
			}
			b.WriteString(num)
			b.WriteString("s")
		default:
			b.WriteString(num)
			b.WriteString(unit)
		}
		i = k
	}
	return b.String(), nil
}

func isDigitRune(r rune) bool { return r >= '0' && r <= '9' }
func isUnitRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == 'µ' || r == 'μ'
}
