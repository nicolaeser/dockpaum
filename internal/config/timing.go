package config

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nicolaeser/dockpaum/internal/window"
)

var analysisStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func (c *Config) timingWarnings() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	regular, interval := c.regularSchedule()
	global := compileWindowSet(c.Schedule.Timezone, c.Schedule.Windows)
	if !global.Empty() {
		if window.LongestOpening(analysisStart, global) == 0 {
			add("schedule.windows: never open (every time range is empty), so no update run ever starts")
			return out
		}
		if m, missed := scheduleMiss(regular, interval, global); missed {
			key := "schedule.interval"
			if regular != nil {
				key = "schedule.cron"
			}
			add("%s: %s", key, m.explain("schedule.windows",
				"no update run ever starts — move the cron or the windows",
				"the runs can keep missing the windows — use schedule.cron with a time inside them"))
		}
	}

	for i, p := range c.Update.Policies {
		if p.Mode == ModeNone {
			continue
		}
		label := fmt.Sprintf("update.policies[%d] (id=%q)", i, p.ID)
		own := compileWindowSet(p.Timezone, p.Windows)
		if !own.Empty() {
			if window.LongestOpening(analysisStart, own) == 0 {
				add("%s.windows: never open (every time range is empty), so containers using this policy are never updated", label)
				continue
			}
			if !global.Empty() && window.LongestOpening(analysisStart, global, own) == 0 {
				add("%s.windows: never open while schedule.windows is open, so containers using this policy are never updated", label)
				continue
			}
		}
		if p.Schedule != "" {
			where := windowsName(global, own)
			sched, err := ParseCron(p.CronSpec())
			if where == "" || err != nil {
				continue
			}
			if m, missed := scheduleMiss(sched, 0, global, own); missed {
				add("%s.schedule: %s", label, m.explain(where,
					"containers using this policy are never updated",
					"the runs can keep missing the windows — use a cron with a time inside them"))
			}
			continue
		}
		if own.Empty() {
			continue
		}
		if m, missed := scheduleMiss(regular, interval, global, own); missed {
			key := "schedule.interval"
			if regular != nil {
				key = "schedule.cron"
			}
			add("%s.windows: the regular schedule (%s) %s", label, key, m.explain("the policy windows",
				"containers using this policy are never updated — give the policy its own schedule inside its windows",
				"the runs can keep missing the windows — give the policy its own schedule inside them"))
		}
	}
	return out
}

func (c *Config) regularSchedule() (cron.Schedule, time.Duration) {
	if c.Schedule.Cron == "" {
		return nil, c.Schedule.Interval.Std()
	}
	sched, err := ParseCron(c.Schedule.CronSpec())
	if err != nil {
		return nil, 0
	}
	return sched, 0
}

type miss struct {
	never          bool
	every, longest time.Duration
}

func (m miss) explain(windows, never, every string) string {
	if m.never {
		return fmt.Sprintf("never fires inside %s, so %s", windows, never)
	}
	return fmt.Sprintf("runs every %s, longer than the longest opening of %s (%s), so %s", formatDuration(m.every), windows, formatDuration(m.longest), every)
}

func scheduleMiss(sched cron.Schedule, interval time.Duration, sets ...*window.Set) (miss, bool) {
	if every, ok := sched.(cron.ConstantDelaySchedule); ok {
		sched, interval = nil, every.Delay
	}
	if sched != nil {
		return miss{never: true}, !firesInside(sched, sets...)
	}
	if interval <= 0 {
		return miss{}, false
	}
	longest := window.LongestOpening(analysisStart, sets...)
	return miss{every: interval, longest: longest}, interval > longest
}

func windowsName(global, own *window.Set) string {
	switch {
	case !global.Empty() && !own.Empty():
		return "the policy windows while schedule.windows is open"
	case !own.Empty():
		return "the policy windows"
	case !global.Empty():
		return "schedule.windows (which gate policy schedules too)"
	}
	return ""
}

func firesInside(sched cron.Schedule, sets ...*window.Set) bool {
	t := analysisStart
	limit := analysisStart.AddDate(4, 0, 1)
	for range 50000 {
		t = sched.Next(t)
		if t.IsZero() || t.After(limit) {
			return false
		}
		inside := true
		for _, s := range sets {
			if !s.Allows(t) {
				inside = false
				break
			}
		}
		if inside {
			return true
		}
	}
	return true
}

func compileWindowSet(tz string, specs []WindowSpec) *window.Set {
	if len(specs) == 0 {
		return window.AlwaysAllowed()
	}
	ws := make([]window.Spec, len(specs))
	for i, s := range specs {
		ws[i] = window.Spec{Days: s.Days, Times: s.Times}
	}
	set, err := window.Compile(tz, ws)
	if err != nil {
		return window.AlwaysAllowed()
	}
	return set
}
