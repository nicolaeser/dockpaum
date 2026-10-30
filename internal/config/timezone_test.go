package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tzJobs = "jobs:\n  - name: j\n    cron: \"0 2 * * *\"\n    commands: [\"true\"]\n"

func policyByID(t *testing.T, c *Config, id string) Policy {
	t.Helper()
	for _, p := range c.Update.Policies {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no policy %q in %+v", id, c.Update.Policies)
	return Policy{}
}

func TestTimezoneInheritance(t *testing.T) {
	isolateEnv(t)
	t.Setenv("TZ", "")
	tests := []struct {
		name                                     string
		yaml                                     string
		global, schedule, web, fallback, jobZone string
	}{
		{
			name:   "nothing set: UTC everywhere",
			yaml:   "update:\n  policies:\n    - id: web\n" + tzJobs,
			global: "UTC", schedule: "UTC", web: "UTC", fallback: "UTC", jobZone: "UTC",
		},
		{
			name:   "top-level timezone is inherited by everything",
			yaml:   "timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: web\n" + tzJobs,
			global: "Europe/Berlin", schedule: "Europe/Berlin", web: "Europe/Berlin", fallback: "Europe/Berlin", jobZone: "Europe/Berlin",
		},
		{
			name:   "schedule.timezone alone acts as the global zone",
			yaml:   "schedule:\n  timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: web\n" + tzJobs,
			global: "Europe/Berlin", schedule: "Europe/Berlin", web: "Europe/Berlin", fallback: "Europe/Berlin", jobZone: "Europe/Berlin",
		},
		{
			name:   "schedule.timezone overrides only the schedule",
			yaml:   "timezone: UTC\nschedule:\n  timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: web\n" + tzJobs,
			global: "UTC", schedule: "Europe/Berlin", web: "UTC", fallback: "UTC", jobZone: "UTC",
		},
		{
			name: "policy and job overrides win",
			yaml: "timezone: Europe/Berlin\nupdate:\n  policies:\n    - id: web\n      timezone: America/New_York\n" +
				"jobs:\n  - name: j\n    cron: \"0 2 * * *\"\n    timezone: Asia/Tokyo\n    commands: [\"true\"]\n",
			global: "Europe/Berlin", schedule: "Europe/Berlin", web: "America/New_York", fallback: "Europe/Berlin", jobZone: "Asia/Tokyo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := decode([]byte(tt.yaml), tt.name)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{c.Timezone, c.Schedule.Timezone, policyByID(t, c, "web").Timezone, policyByID(t, c, "default").Timezone, c.Jobs[0].Timezone}
			want := []string{tt.global, tt.schedule, tt.web, tt.fallback, tt.jobZone}
			for i, field := range []string{"timezone", "schedule.timezone", "policy web", "default policy", "job"} {
				if got[i] != want[i] {
					t.Errorf("%s zone = %q, want %q", field, got[i], want[i])
				}
			}
			if spec := c.Jobs[0].CronSpec(); spec != "CRON_TZ="+tt.jobZone+" 0 2 * * *" {
				t.Errorf("job CronSpec = %q, want the job zone applied", spec)
			}
		})
	}
}

func TestTimezoneFromEnv(t *testing.T) {
	isolateEnv(t)
	root := t.TempDir()
	berlin := filepath.Join(root, "zoneinfo", "Europe", "Berlin")
	posixTokyo := filepath.Join(root, "zoneinfo", "posix", "Asia", "Tokyo")
	for _, f := range []string{berlin, posixTokyo} {
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("TZif"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	localtime := filepath.Join(root, "localtime")
	if err := os.Symlink(berlin, localtime); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(root, "copied-localtime")
	if err := os.WriteFile(copied, []byte("TZif"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		tz, want string
		warns    bool
	}{
		{tz: "", want: "UTC"},
		{tz: "America/New_York", want: "America/New_York"},
		{tz: ":Europe/Berlin", want: "Europe/Berlin"},
		{tz: "Not/AZone", want: "UTC", warns: true},
		{tz: berlin, want: "Europe/Berlin"},
		{tz: ":" + localtime, want: "Europe/Berlin"},
		{tz: posixTokyo, want: "Asia/Tokyo"},
		{tz: ":" + copied, want: "UTC", warns: true},
		{tz: filepath.Join(root, "zoneinfo", "Not", "AZone"), want: "UTC", warns: true},
	}
	for _, tt := range tests {
		t.Run(tt.tz, func(t *testing.T) {
			t.Setenv("TZ", tt.tz)
			c, err := decode([]byte("schedule:\n  cron: \"0 4 * * *\"\n"), "env")
			if err != nil {
				t.Fatal(err)
			}
			if c.Timezone != tt.want || c.Schedule.Timezone != tt.want {
				t.Errorf("TZ=%q: zones = %q/%q, want %q", tt.tz, c.Timezone, c.Schedule.Timezone, tt.want)
			}
			if spec := c.Schedule.CronSpec(); spec != "CRON_TZ="+tt.want+" 0 4 * * *" {
				t.Errorf("TZ=%q: CronSpec = %q", tt.tz, spec)
			}
			warned := strings.Contains(strings.Join(c.Warnings(), "\n"), "TZ=")
			if warned != tt.warns {
				t.Errorf("TZ=%q: warned = %v, want %v (%v)", tt.tz, warned, tt.warns, c.Warnings())
			}
		})
	}

	t.Run("an explicit zone ignores TZ", func(t *testing.T) {
		t.Setenv("TZ", "Not/AZone")
		c, err := decode([]byte("timezone: Asia/Tokyo\n"), "explicit")
		if err != nil {
			t.Fatal(err)
		}
		if c.Timezone != "Asia/Tokyo" || len(c.Warnings()) != 0 {
			t.Errorf("zone = %q, warnings = %v; want Asia/Tokyo and no TZ warning", c.Timezone, c.Warnings())
		}
	})
}

func TestTimezoneErrorsNameTheirKey(t *testing.T) {
	isolateEnv(t)
	tests := []struct {
		yaml, path string
	}{
		{"timezone: Mars/Olympus\n", "timezone"},
		{"schedule:\n  timezone: Mars/Olympus\n", "schedule.timezone"},
		{"timezone: UTC\nschedule:\n  timezone: Mars/Olympus\n", "schedule.timezone"},
		{"update:\n  policies:\n    - id: web\n      timezone: Mars/Olympus\n", `update.policies[0] (id="web").timezone`},
		{"jobs:\n  - name: j\n    cron: \"0 2 * * *\"\n    timezone: Mars/Olympus\n    commands: [\"true\"]\n", "jobs[0] (j).timezone"},
	}
	for _, tt := range tests {
		_, err := decode([]byte(tt.yaml), "tz")
		if err == nil || !strings.Contains(err.Error(), ": "+tt.path+": unknown time zone") {
			t.Errorf("decode(%q) = %v, want an unknown time zone error for %s", tt.yaml, err, tt.path)
		}
	}
}
