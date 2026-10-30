package window

import (
	"fmt"
	"strings"
	"time"
)

type Spec struct {
	Days  []string `yaml:"days"`
	Times []string `yaml:"times"`
}

type compiled struct {
	days  map[time.Weekday]struct{}
	times []timeRange
}

type timeRange struct {
	startMin int
	endMin   int
}

type Set struct {
	loc     *time.Location
	windows []compiled
}

func Compile(timezone string, specs []Spec) (*Set, error) {
	loc := time.Local
	if timezone != "" {
		l, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone: %w", err)
		}
		loc = l
	}
	out := &Set{loc: loc}
	for i, w := range specs {
		days, err := parseDays(w.Days)
		if err != nil {
			return nil, fmt.Errorf("windows[%d].days: %w", i, err)
		}
		times, err := parseTimes(w.Times)
		if err != nil {
			return nil, fmt.Errorf("windows[%d].times: %w", i, err)
		}
		out.windows = append(out.windows, compiled{days: days, times: times})
	}
	return out, nil
}

func AlwaysAllowed() *Set { return &Set{loc: time.Local} }

func (s *Set) Allows(now time.Time) bool {
	if s == nil || len(s.windows) == 0 {
		return true
	}
	now = now.In(s.loc)
	day := now.Weekday()
	prevDay := (day + 6) % 7
	mins := now.Hour()*60 + now.Minute()
	for _, w := range s.windows {
		_, dayOK := w.days[day]
		_, prevOK := w.days[prevDay]
		if !dayOK && !prevOK {
			continue
		}
		for _, r := range w.times {
			if r.endMin > r.startMin {
				if dayOK && mins >= r.startMin && mins < r.endMin {
					return true
				}
				continue
			}
			if r.endMin == r.startMin {
				continue
			}
			if dayOK && mins >= r.startMin {
				return true
			}
			if prevOK && mins < r.endMin {
				return true
			}
		}
	}
	return false
}

func (s *Set) Empty() bool { return s == nil || len(s.windows) == 0 }

const Week = 7 * 24 * time.Hour

func LongestOpening(from time.Time, sets ...*Set) time.Duration {
	var active []*Set
	for _, s := range sets {
		if !s.Empty() {
			active = append(active, s)
		}
	}
	if len(active) == 0 {
		return Week
	}
	open := func(t time.Time) bool {
		for _, s := range active {
			if !s.Allows(t) {
				return false
			}
		}
		return true
	}
	from = from.Truncate(time.Minute)
	const minutes = int(Week / time.Minute)
	start := -1
	for m := 0; m < minutes; m++ {
		if !open(from.Add(time.Duration(m) * time.Minute)) {
			start = m
			break
		}
	}
	if start < 0 {
		return Week
	}
	longest, run := 0, 0
	for m := 1; m <= minutes; m++ {
		if open(from.Add(time.Duration(start+m) * time.Minute)) {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return time.Duration(longest) * time.Minute
}

func (s *Set) Next(after time.Time) time.Time {
	if s.Empty() {
		return time.Time{}
	}
	if s.Allows(after) {
		return after
	}
	loc := s.Location()
	horizon := after.Add(Week + 24*time.Hour)
	var best time.Time
	consider := func(t time.Time) {
		if t.After(after) && !t.After(horizon) && (best.IsZero() || t.Before(best)) && s.Allows(t) {
			best = t
		}
	}
	for from := after; from.Before(horizon); {
		local := from.In(loc)
		_, offset := local.Zone()
		_, end := local.ZoneBounds()
		if end.IsZero() || end.After(horizon) {
			end = horizon
		}
		shift := time.Duration(offset) * time.Second
		y, m, d := from.Add(shift).UTC().Date()
		days := int(end.Sub(from)/(24*time.Hour)) + 1
		for i := 0; i <= days; i++ {
			at := func(minute int) {
				t := time.Date(y, m, d+i, 0, minute, 0, 0, time.UTC).Add(-shift)
				if !t.Before(from) && t.Before(end) {
					consider(t)
				}
			}
			at(0)
			for _, w := range s.windows {
				for _, r := range w.times {
					at(r.startMin)
				}
			}
		}
		consider(end)
		from = end
	}
	if best.IsZero() {
		return best
	}
	return best.In(after.Location())
}

func (s *Set) Location() *time.Location {
	if s == nil || s.loc == nil {
		return time.Local
	}
	return s.loc
}

var dayAliases = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

func parseDays(in []string) (map[time.Weekday]struct{}, error) {
	out := map[time.Weekday]struct{}{}
	if len(in) == 0 {
		for d := time.Sunday; d <= time.Saturday; d++ {
			out[d] = struct{}{}
		}
		return out, nil
	}
	for _, raw := range in {
		s := strings.ToLower(strings.TrimSpace(raw))
		switch s {
		case "all", "*", "any", "daily":
			for d := time.Sunday; d <= time.Saturday; d++ {
				out[d] = struct{}{}
			}
			continue
		case "weekdays", "weekday":
			for d := time.Monday; d <= time.Friday; d++ {
				out[d] = struct{}{}
			}
			continue
		case "weekends", "weekend":
			out[time.Saturday] = struct{}{}
			out[time.Sunday] = struct{}{}
			continue
		}
		if i := strings.Index(s, "-"); i > 0 {
			a, ok1 := dayAliases[strings.TrimSpace(s[:i])]
			b, ok2 := dayAliases[strings.TrimSpace(s[i+1:])]
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("unknown day in range %q", raw)
			}
			d := a
			for {
				out[d] = struct{}{}
				if d == b {
					break
				}
				d = (d + 1) % 7
			}
			continue
		}
		d, ok := dayAliases[s]
		if !ok {
			return nil, fmt.Errorf("unknown day %q", raw)
		}
		out[d] = struct{}{}
	}
	return out, nil
}

func parseTimes(in []string) ([]timeRange, error) {
	if len(in) == 0 {
		return []timeRange{{startMin: 0, endMin: 24 * 60}}, nil
	}
	out := make([]timeRange, 0, len(in))
	for _, raw := range in {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "always" || s == "all" || s == "*" {
			out = append(out, timeRange{startMin: 0, endMin: 24 * 60})
			continue
		}
		parts := strings.SplitN(s, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("expected HH:MM-HH:MM, got %q", raw)
		}
		start, err := parseHM(parts[0])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", raw, err)
		}
		end, err := parseHM(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", raw, err)
		}
		if end == 0 {
			end = 24 * 60
		}
		out = append(out, timeRange{startMin: start, endMin: end})
	}
	return out, nil
}

func parseHM(s string) (int, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	h, err := parseTimePart(parts[0])
	if err != nil {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := parseTimePart(parts[1])
	if err != nil {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	if h > 24 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("out of range %q", s)
	}
	return h*60 + m, nil
}

func parseTimePart(s string) (int, error) {
	if len(s) == 0 || len(s) > 2 {
		return 0, fmt.Errorf("bad number %q", s)
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
