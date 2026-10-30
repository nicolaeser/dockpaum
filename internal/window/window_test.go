package window

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func daySet(days ...time.Weekday) map[time.Weekday]struct{} {
	out := map[time.Weekday]struct{}{}
	for _, d := range days {
		out[d] = struct{}{}
	}
	return out
}

func allDays() map[time.Weekday]struct{} {
	return daySet(time.Sunday, time.Monday, time.Tuesday, time.Wednesday,
		time.Thursday, time.Friday, time.Saturday)
}

func TestParseDays(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[time.Weekday]struct{}
		wantErr bool
	}{
		{
			name: "single day",
			in:   []string{"mon"},
			want: daySet(time.Monday),
		},
		{
			name: "multiple single days",
			in:   []string{"tue", "thu"},
			want: daySet(time.Tuesday, time.Thursday),
		},
		{
			name: "long names and mixed case",
			in:   []string{"Monday", "FRIDAY"},
			want: daySet(time.Monday, time.Friday),
		},
		{
			name: "weekdays alias",
			in:   []string{"weekdays"},
			want: daySet(time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday),
		},
		{
			name: "weekends alias",
			in:   []string{"weekends"},
			want: daySet(time.Saturday, time.Sunday),
		},
		{
			name: "all alias",
			in:   []string{"all"},
			want: allDays(),
		},
		{
			name: "star alias",
			in:   []string{"*"},
			want: allDays(),
		},
		{
			name: "simple range",
			in:   []string{"mon-wed"},
			want: daySet(time.Monday, time.Tuesday, time.Wednesday),
		},
		{
			name: "wrap-around range fri-mon",
			in:   []string{"fri-mon"},
			want: daySet(time.Friday, time.Saturday, time.Sunday, time.Monday),
		},
		{
			name: "empty means every day",
			in:   nil,
			want: allDays(),
		},
		{
			name:    "unknown day",
			in:      []string{"funday"},
			wantErr: true,
		},
		{
			name:    "unknown day in range",
			in:      []string{"mon-funday"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDays(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDays(%v) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDays(%v): unexpected error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseDays(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseHM(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "00:00", want: 0},
		{in: "23:59", want: 23*60 + 59},
		{in: "24:00", want: 24 * 60},
		{in: "9:05", want: 9*60 + 5},
		{in: " 12:30 ", want: 12*60 + 30},

		{in: "24:59", wantErr: true},
		{in: "25:00", wantErr: true},
		{in: "12:60", wantErr: true},
		{in: "12:5x", wantErr: true},
		{in: "1:2:3", wantErr: true},
		{in: "12", wantErr: true},
		{in: "12:30x", wantErr: true},
		{in: "x12:30", wantErr: true},
		{in: ":30", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseHM(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseHM(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHM(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseHM(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseTimes(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []timeRange
		wantErr bool
	}{
		{
			name: "until midnight",
			in:   []string{"22:00-00:00"},
			want: []timeRange{{startMin: 22 * 60, endMin: 24 * 60}},
		},
		{
			name: "00:00-00:00 means whole day",
			in:   []string{"00:00-00:00"},
			want: []timeRange{{startMin: 0, endMin: 24 * 60}},
		},
		{
			name: "empty means whole day",
			in:   nil,
			want: []timeRange{{startMin: 0, endMin: 24 * 60}},
		},
		{
			name: "always alias",
			in:   []string{"always"},
			want: []timeRange{{startMin: 0, endMin: 24 * 60}},
		},
		{
			name: "plain range",
			in:   []string{"02:00-04:30"},
			want: []timeRange{{startMin: 2 * 60, endMin: 4*60 + 30}},
		},
		{
			name:    "missing dash",
			in:      []string{"02:00"},
			wantErr: true,
		},
		{
			name:    "bad start",
			in:      []string{"25:00-26:00"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTimes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTimes(%v) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTimes(%v): unexpected error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseTimes(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestAllowsOvernightDayAttribution(t *testing.T) {
	if d := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC).Weekday(); d != time.Saturday {
		t.Fatalf("2026-06-13 is %v, expected Saturday", d)
	}
	if d := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC).Weekday(); d != time.Sunday {
		t.Fatalf("2026-06-14 is %v, expected Sunday", d)
	}

	set, err := Compile("UTC", []Spec{{Days: []string{"sat"}, Times: []string{"22:00-02:00"}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{
			name: "saturday 23:00 inside pre-midnight part",
			now:  time.Date(2026, 6, 13, 23, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "saturday 22:00 inclusive start",
			now:  time.Date(2026, 6, 13, 22, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "sunday 01:00 inside post-midnight spill-over",
			now:  time.Date(2026, 6, 14, 1, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			name: "saturday 01:00 belongs to friday night, denied",
			now:  time.Date(2026, 6, 13, 1, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "sunday 03:00 past window end, denied",
			now:  time.Date(2026, 6, 14, 3, 0, 0, 0, time.UTC),
			want: false,
		},
		{
			name: "sunday 02:00 exclusive end, denied",
			now:  time.Date(2026, 6, 14, 2, 0, 0, 0, time.UTC),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := set.Allows(tt.now); got != tt.want {
				t.Errorf("Allows(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestAllowsTimezone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	set, err := Compile("Europe/Berlin", []Spec{{Days: []string{"mon"}, Times: []string{"02:00-04:00"}}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	mondayBerlin := time.Date(2026, 6, 15, 3, 0, 0, 0, berlin)
	if d := mondayBerlin.Weekday(); d != time.Monday {
		t.Fatalf("2026-06-15 is %v in Berlin, expected Monday", d)
	}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{
			name: "UTC instant for monday 03:00 berlin is allowed",
			now:  mondayBerlin.UTC(),
			want: true,
		},
		{
			name: "monday 05:00 berlin is outside the window",
			now:  time.Date(2026, 6, 15, 5, 0, 0, 0, berlin).UTC(),
			want: false,
		},
		{
			name: "sunday 03:00 berlin is the wrong day",
			now:  time.Date(2026, 6, 14, 3, 0, 0, 0, berlin).UTC(),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := set.Allows(tt.now); got != tt.want {
				t.Errorf("Allows(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestEmptyWindowsAlwaysAllow(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 15, 12, 34, 0, 0, time.UTC),
	}

	empty, err := Compile("UTC", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	sets := map[string]*Set{
		"compiled empty": empty,
		"AlwaysAllowed":  AlwaysAllowed(),
		"nil set":        nil,
	}
	for name, s := range sets {
		t.Run(name, func(t *testing.T) {
			if !s.Empty() {
				t.Errorf("Empty() = false, want true")
			}
			for _, now := range instants {
				if !s.Allows(now) {
					t.Errorf("Allows(%v) = false, want true", now)
				}
			}
		})
	}
}

func TestLocation(t *testing.T) {
	berlin, err := Compile("Europe/Berlin", nil)
	if err != nil {
		t.Fatal(err)
	}
	local, err := Compile("", nil)
	if err != nil {
		t.Fatal(err)
	}
	var none *Set
	for name, tt := range map[string]struct {
		set  *Set
		want string
	}{
		"compiled zone": {berlin, "Europe/Berlin"},
		"no zone":       {local, time.Local.String()},
		"AlwaysAllowed": {AlwaysAllowed(), time.Local.String()},
		"nil set":       {none, time.Local.String()},
	} {
		if got := tt.set.Location().String(); got != tt.want {
			t.Errorf("%s: Location() = %q, want %q", name, got, tt.want)
		}
	}
}

func TestLongestOpening(t *testing.T) {
	monday := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mustCompile := func(tz string, specs ...Spec) *Set {
		t.Helper()
		s, err := Compile(tz, specs)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name string
		sets []*Set
		want time.Duration
	}{
		{"no sets", nil, Week},
		{"empty sets always allow", []*Set{nil, AlwaysAllowed()}, Week},
		{"daily hour", []*Set{mustCompile("UTC", Spec{Times: []string{"03:00-04:00"}})}, time.Hour},
		{"longest of several", []*Set{mustCompile("UTC", Spec{Times: []string{"03:00-03:30", "12:00-14:00"}})}, 2 * time.Hour},
		{"overnight across midnight", []*Set{mustCompile("UTC", Spec{Days: []string{"sat"}, Times: []string{"22:00-02:00"}})}, 4 * time.Hour},
		{"opening across the start of the scan", []*Set{mustCompile("UTC", Spec{Days: []string{"sun"}, Times: []string{"22:00-02:00"}})}, 4 * time.Hour},
		{"all day on weekends joins saturday and sunday", []*Set{mustCompile("UTC", Spec{Days: []string{"weekends"}})}, 48 * time.Hour},
		{"every day all day", []*Set{mustCompile("UTC", Spec{Days: []string{"all"}})}, Week},
		{"empty range never opens", []*Set{mustCompile("UTC", Spec{Times: []string{"05:00-05:00"}})}, 0},
		{"intersection", []*Set{
			mustCompile("UTC", Spec{Times: []string{"02:00-05:00"}}),
			mustCompile("UTC", Spec{Times: []string{"04:00-06:00"}}),
		}, time.Hour},
		{"disjoint sets never open together", []*Set{
			mustCompile("UTC", Spec{Days: []string{"mon"}}),
			mustCompile("UTC", Spec{Days: []string{"tue"}}),
		}, 0},
		{"zones are honored", []*Set{
			mustCompile("UTC", Spec{Times: []string{"02:00-04:00"}}),
			mustCompile("Europe/Berlin", Spec{Times: []string{"04:00-06:00"}}),
		}, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LongestOpening(monday, tt.sets...); got != tt.want {
				t.Errorf("LongestOpening = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustCompile(t *testing.T, tz string, specs ...Spec) *Set {
	t.Helper()
	s, err := Compile(tz, specs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNext(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		t.Helper()
		loc := berlin
		if strings.HasSuffix(s, "Z") {
			s, loc = strings.TrimSuffix(s, "Z"), time.UTC
		}
		v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name  string
		specs []Spec
		after string
		want  string
	}{
		{"inside a window", []Spec{{Days: []string{"mon"}, Times: []string{"09:00-17:00"}}}, "2024-03-04 12:00:00", "2024-03-04 12:00:00"},
		{"later that day", []Spec{{Days: []string{"mon"}, Times: []string{"09:00-17:00"}}}, "2024-03-04 08:59:30", "2024-03-04 09:00:00"},
		{"next week after it closed", []Spec{{Days: []string{"mon"}, Times: []string{"09:00-17:00"}}}, "2024-03-04 17:00:00", "2024-03-11 09:00:00"},
		{"the earliest of several", []Spec{{Days: []string{"fri"}, Times: []string{"01:00-02:00"}}, {Days: []string{"weekdays"}, Times: []string{"22:00-23:00", "03:00-04:00"}}}, "2024-03-04 04:30:00", "2024-03-04 22:00:00"},
		{"overnight range, before midnight", []Spec{{Days: []string{"sat"}, Times: []string{"22:00-02:00"}}}, "2024-03-04 10:00:00", "2024-03-09 22:00:00"},
		{"overnight range, the morning after", []Spec{{Days: []string{"sat"}, Times: []string{"22:00-02:00"}}}, "2024-03-10 01:30:00", "2024-03-10 01:30:00"},
		{"whole days", []Spec{{Days: []string{"weekends"}}}, "2024-03-08 23:59:59", "2024-03-09 00:00:00"},
		{"spring forward into a window", []Spec{{Days: []string{"sun"}, Times: []string{"02:30-04:00"}}}, "2024-03-31 00:00:00", "2024-03-31 03:00:00"},
		{"a window that the clock skips", []Spec{{Days: []string{"sun"}, Times: []string{"02:10-02:50"}}}, "2024-03-31 00:00:00", "2024-04-07 02:10:00"},
		{"fall back: the first 02:30", []Spec{{Days: []string{"sun"}, Times: []string{"02:30-02:45"}}}, "2024-10-27 00:00:00", "2024-10-27 00:30:00Z"},
		{"fall back: the repeated 02:30", []Spec{{Days: []string{"sun"}, Times: []string{"02:30-02:45"}}}, "2024-10-27 00:50:00Z", "2024-10-27 01:30:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustCompile(t, "Europe/Berlin", tt.specs...)
			after, want := at(tt.after), at(tt.want)
			if got := s.Next(after); !got.Equal(want) {
				t.Errorf("Next(%s) = %s, want %s", after, got.In(berlin), want)
			}
		})
	}
}

func TestNextKeepsTheLocationOfAfter(t *testing.T) {
	s := mustCompile(t, "Europe/Berlin", Spec{Days: []string{"mon"}, Times: []string{"09:00-17:00"}})
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	open := time.Date(2024, 3, 4, 12, 0, 0, 0, time.UTC)
	closed := time.Date(2024, 3, 4, 20, 0, 0, 0, time.UTC)
	for _, loc := range []*time.Location{time.UTC, newYork} {
		for _, after := range []time.Time{open.In(loc), closed.In(loc)} {
			if got := s.Next(after); got.Location() != loc {
				t.Errorf("Next(%s) = %s in %s, want it in %s whether the window is open or not", after, got, got.Location(), loc)
			}
		}
	}
}

func TestNextWithoutAnOpening(t *testing.T) {
	now := time.Date(2024, 3, 4, 12, 0, 0, 0, time.UTC)
	var nilSet *Set
	for name, s := range map[string]*Set{
		"nil":            nilSet,
		"always allowed": AlwaysAllowed(),
		"no windows":     mustCompile(t, "UTC"),
		"never opens":    mustCompile(t, "UTC", Spec{Times: []string{"10:00-10:00"}}),
	} {
		if got := s.Next(now); !got.IsZero() {
			t.Errorf("%s: Next = %s, want the zero time", name, got)
		}
	}
}

func TestNextMatchesAMinuteScan(t *testing.T) {
	sets := map[string][]Spec{
		"office hours":   {{Days: []string{"Mon-Fri"}, Times: []string{"08:00-12:00", "13:00-17:30"}}},
		"overnight":      {{Days: []string{"sat", "wed"}, Times: []string{"23:30-01:15"}}},
		"dst hours":      {{Days: []string{"sun"}, Times: []string{"02:15-03:30"}}},
		"late and short": {{Days: []string{"thu"}, Times: []string{"23:59-00:00"}}, {Days: []string{"sun"}, Times: []string{"00:00-00:01"}}},
	}
	for _, tz := range []string{"Europe/Berlin", "America/New_York", "UTC"} {
		for name, specs := range sets {
			s := mustCompile(t, tz, specs...)
			for _, start := range []time.Time{
				time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC),
				time.Date(2024, 3, 27, 0, 0, 0, 0, time.UTC),
				time.Date(2024, 10, 24, 0, 0, 0, 0, time.UTC),
				time.Date(2024, 10, 31, 0, 0, 0, 0, time.UTC),
			} {
				var openings []time.Time
				for m := start; m.Before(start.Add(3 * Week)); m = m.Add(time.Minute) {
					if s.Allows(m) && !s.Allows(m.Add(-time.Minute)) {
						openings = append(openings, m)
					}
				}
				for after := start; after.Before(start.Add(Week)); after = after.Add(97*time.Minute + 13*time.Second) {
					want := after
					if !s.Allows(after) {
						i, _ := slices.BinarySearchFunc(openings, after, func(o, a time.Time) int { return o.Compare(a) })
						want = openings[i]
					}
					if got := s.Next(after); !got.Equal(want) {
						t.Fatalf("%s %s: Next(%s) = %s, want %s", tz, name, after, got, want)
					}
				}
			}
		}
	}
}
