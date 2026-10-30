package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (c *Config) resolveTimezones(tz string) error {
	type zone struct{ path, name string }
	zones := []zone{{"timezone", c.Timezone}, {"schedule.timezone", c.Schedule.Timezone}}
	for i, p := range c.Update.Policies {
		zones = append(zones, zone{fmt.Sprintf("update.policies[%d] (id=%q).timezone", i, p.ID), p.Timezone})
	}
	for i, j := range c.Jobs {
		zones = append(zones, zone{fmt.Sprintf("jobs[%d] (%s).timezone", i, j.Name), j.Timezone})
	}
	for _, z := range zones {
		if z.name == "" {
			continue
		}
		if _, err := time.LoadLocation(z.name); err != nil {
			return fmt.Errorf("%s: %w", z.path, err)
		}
	}

	global := c.Timezone
	if global == "" {
		global = c.Schedule.Timezone
	}
	if global == "" {
		var warning string
		global, warning = envTimezone(tz)
		if warning != "" {
			c.loadWarnings = append(c.loadWarnings, warning)
		}
	}
	c.Timezone = global
	if c.Schedule.Timezone == "" {
		c.Schedule.Timezone = global
	}
	for i := range c.Update.Policies {
		if c.Update.Policies[i].Timezone == "" {
			c.Update.Policies[i].Timezone = global
		}
	}
	for i := range c.Jobs {
		if c.Jobs[i].Timezone == "" {
			c.Jobs[i].Timezone = global
		}
	}
	return nil
}

func envTimezone(tz string) (zone, warning string) {
	tz = strings.TrimPrefix(strings.TrimSpace(tz), ":")
	if tz == "" {
		return "UTC", ""
	}
	if filepath.IsAbs(tz) {
		name := zoneFromPath(tz)
		if name == "" {
			return "UTC", fmt.Sprintf("TZ=%q is not a zoneinfo file dockpaum can name (a path under a zoneinfo directory, or a link to one): schedules and windows use UTC — set timezone: in the config", tz)
		}
		tz = name
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC", fmt.Sprintf("TZ=%q is not a time zone dockpaum can load (%v): schedules and windows use UTC — set timezone: in the config", tz, err)
	}
	return tz, ""
}

func zoneFromPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.ToSlash(filepath.Clean(path))
	const dir = "/zoneinfo/"
	i := strings.LastIndex(path, dir)
	if i < 0 {
		return ""
	}
	return strings.TrimPrefix(path[i+len(dir):], "posix/")
}
