package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const EnvConfigPath = "DOCKPAUM_CONFIG"

const DefaultConfigPath = "/etc/dockpaum/dockpaum.yml"

func ResolvePath(flagValue string, flagSet bool) string {
	if flagSet && strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	if p := strings.TrimSpace(os.Getenv(EnvConfigPath)); p != "" {
		return p
	}
	if p := strings.TrimSpace(flagValue); p != "" {
		return p
	}
	return DefaultConfigPath
}

const (
	envDryRun         = "DOCKPAUM_DRY_RUN"
	envSchedule       = "DOCKPAUM_SCHEDULE"
	envZone           = "DOCKPAUM_TIMEZONE"
	envLogLevel       = "DOCKPAUM_LOG_LEVEL"
	envLogFormat      = "DOCKPAUM_LOG_FORMAT"
	envHTTPEnabled    = "DOCKPAUM_HTTP_ENABLED"
	envHTTPPort       = "DOCKPAUM_HTTP_PORT"
	envHTTPAuth       = "DOCKPAUM_HTTP_TOKEN"
	envHTTPReadAuth   = "DOCKPAUM_HTTP_READ_TOKEN"
	envHTTPUpdateAPI  = "DOCKPAUM_HTTP_UPDATE_API"
	envNotifyURLs     = "DOCKPAUM_NOTIFY_URLS"
	envNotifyEvents   = "DOCKPAUM_NOTIFY_EVENTS"
	envLabelEnable    = "DOCKPAUM_LABEL_ENABLE"
	envScope          = "DOCKPAUM_SCOPE"
	envCleanup        = "DOCKPAUM_CLEANUP"
	envIncludeStopped = "DOCKPAUM_INCLUDE_STOPPED"
	envMode           = "DOCKPAUM_MODE"
	envMinImageAge    = "DOCKPAUM_MIN_IMAGE_AGE"
	envHealthTimeout  = "DOCKPAUM_HEALTH_TIMEOUT"
	envRollback       = "DOCKPAUM_ROLLBACK"
	envSelfUpdate     = "DOCKPAUM_SELF_UPDATE"
	envInstance       = "DOCKPAUM_INSTANCE"
	envStateDir       = "DOCKPAUM_STATE_DIR"
	envWatchtowerEnv  = "DOCKPAUM_WATCHTOWER_ENV"

	fileSuffix = "_FILE"
)

var knownDockpaumEnv = []string{
	EnvConfigPath, EnvConfigURL, EnvInsecureConfigURL,
	envDryRun, envSchedule, envZone, envLogLevel, envLogFormat,
	envHTTPEnabled, envHTTPPort, envHTTPAuth, envHTTPAuth + fileSuffix, envHTTPReadAuth, envHTTPReadAuth + fileSuffix, envHTTPUpdateAPI,
	envNotifyURLs, envNotifyURLs + fileSuffix, envNotifyEvents,
	envLabelEnable, envScope, envCleanup, envIncludeStopped,
	envMode, envMinImageAge, envHealthTimeout, envRollback, envSelfUpdate, envWatchtowerEnv,
	envInstance, envStateDir,
}

var notifyEvents = []string{
	"update.success", "update.failure", "update.available", "update.skipped", "update.denied",
	"update.rolled_back", "update.recovered", "check.failed",
	"self_update.started", "self_update.success", "self_update.failure",
	"run.started", "run.completed", "dockpaum.started", "dockpaum.stopping", "notify.test",
}

type EnvOverride struct {
	Var string
	Key string
}

func (c *Config) EnvOverrides() []EnvOverride {
	return append([]EnvOverride(nil), c.envOverrides...)
}

func (c *Config) RunOnce() bool { return c.runOnce }

func IsConfigEnv(name string) bool {
	for _, prefix := range []string{"DOCKPAUM_", "WATCHTOWER_", "DOCKER_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "REPO_USER" || name == "REPO_PASS" || name == "TZ"
}

type envMap map[string]string

func systemEnv() envMap {
	m := envMap{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}
	return m
}

func (e envMap) get(name string) (string, bool) {
	v := strings.TrimSpace(e[name])
	return v, v != ""
}

func (e envMap) has(name string) bool {
	_, ok := e.get(name)
	return ok
}

func (e envMap) names(prefix string) []string {
	var out []string
	for k := range e {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, errors.New("want true or false")
}

type overlay struct {
	c                *Config
	env              envMap
	watchtowerDryRun bool
	watchtowerNotify map[int]bool
	updateAPIFrom    string
}

func (c *Config) applyEnv(env envMap) error {
	o := &overlay{c: c, env: env, watchtowerNotify: map[int]bool{}}
	watchtower, decided := false, false
	if v, ok := env.get(envWatchtowerEnv); ok {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w", envWatchtowerEnv, v, err)
		}
		watchtower, decided = b, true
	}
	if watchtower {
		if err := o.applyWatchtower(); err != nil {
			return err
		}
	} else {
		o.warnWatchtowerIgnored(decided)
	}
	if err := o.applyDockpaum(); err != nil {
		return err
	}
	if err := o.checkUpdateAPI(); err != nil {
		return err
	}
	if err := o.applyDockerEnv(); err != nil {
		return err
	}
	if watchtower {
		o.warnWatchtowerReplacesFile()
		o.watchtowerObserveNote()
	}
	o.warnMisspeltDockpaumEnv()
	return nil
}

func (o *overlay) record(variable, key string) {
	dockpaum := strings.HasPrefix(variable, "DOCKPAUM_")
	if dockpaum && !strings.HasSuffix(key, "[]") {
		o.c.envOverrides = slices.DeleteFunc(o.c.envOverrides, func(e EnvOverride) bool { return e.Key == key })
	}
	o.c.envOverrides = append(o.c.envOverrides, EnvOverride{Var: variable, Key: key})
}

func (o *overlay) warn(format string, args ...any) {
	o.c.loadWarnings = append(o.c.loadWarnings, fmt.Sprintf(format, args...))
}

func envError(variable, value string, err error) error {
	return fmt.Errorf("%s=%q: %w", variable, value, err)
}

func (o *overlay) boolVar(variable, key string, set func(bool)) error {
	v, ok := o.env.get(variable)
	if !ok {
		return nil
	}
	b, err := parseBool(v)
	if err != nil {
		return envError(variable, v, err)
	}
	set(b)
	o.record(variable, key)
	return nil
}

func (o *overlay) durationVar(variable, key string, set func(time.Duration)) error {
	v, ok := o.env.get(variable)
	if !ok {
		return nil
	}
	d, err := ParseDuration(v)
	if err != nil {
		return envError(variable, v, err)
	}
	if d < 0 {
		return envError(variable, v, errors.New("must not be negative"))
	}
	set(d)
	o.record(variable, key)
	return nil
}

func (o *overlay) secretVar(variable string) (value, source string, err error) {
	v, okValue := o.env.get(variable)
	file, okFile := o.env.get(variable + fileSuffix)
	switch {
	case okValue && okFile:
		return "", "", fmt.Errorf("set either %s or %s%s, not both", variable, variable, fileSuffix)
	case okFile:
		s, err := readSecretFile(file)
		if err != nil {
			return "", "", fmt.Errorf("%s%s: %w", variable, fileSuffix, err)
		}
		return s, variable + fileSuffix, nil
	case okValue:
		return v, variable, nil
	}
	return "", "", nil
}

func (o *overlay) applyDockpaum() error {
	c := o.c
	steps := []func() error{
		func() error { return o.boolVar(envDryRun, "dry_run", func(b bool) { c.DryRun = b }) },
		o.applySchedule,
		o.applyTimezone,
		o.applyLog,
		func() error { return o.boolVar(envHTTPEnabled, "http.enabled", func(b bool) { c.HTTP.Enabled = &b }) },
		o.applyHTTPPort,
		func() error { return o.applyToken(envHTTPAuth, "http.token", &c.HTTP.Token, &c.HTTP.TokenFile) },
		func() error {
			return o.applyToken(envHTTPReadAuth, "http.read_token", &c.HTTP.ReadToken, &c.HTTP.ReadTokenFile)
		},
		func() error {
			return o.boolVar(envHTTPUpdateAPI, "http.update_api", func(b bool) {
				c.HTTP.UpdateAPI = b
				o.updateAPIFrom = envHTTPUpdateAPI
			})
		},
		o.applyNotifyURLs,
		o.applyNotifyEvents,
		func() error {
			return o.boolVar(envLabelEnable, "filters.label_enable", func(b bool) { c.Filters.LabelEnable = b })
		},
		func() error {
			if v, ok := o.env.get(envScope); ok {
				c.Filters.Scope = v
				o.record(envScope, "filters.scope")
			}
			return nil
		},
		func() error {
			return o.boolVar(envCleanup, "update.cleanup_images", func(b bool) { c.Update.CleanupImages = b })
		},
		func() error {
			return o.boolVar(envIncludeStopped, "filters.include_stopped", func(b bool) { c.Filters.IncludeStopped = b })
		},
		o.applyMode,
		func() error {
			return o.durationVar(envMinImageAge, "update.policies[default].min_image_age", func(d time.Duration) {
				p := c.defaultPolicy()
				p.MinImageAge, p.minImageAgeSet = Duration(d), true
			})
		},
		func() error {
			return o.durationVar(envHealthTimeout, "update.policies[default].health_timeout", func(d time.Duration) {
				p := c.defaultPolicy()
				p.HealthTimeout, p.healthTimeoutSet = Duration(d), true
			})
		},
		func() error {
			return o.boolVar(envRollback, "update.rollback_on_failure", func(b bool) { c.Update.RollbackOnFail = &b })
		},
		func() error {
			return o.boolVar(envSelfUpdate, "update.self_update.enabled", func(b bool) { c.Update.SelfUpdate.Enabled = &b })
		},
		o.applyInstance,
		o.applyStateDir,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) defaultPolicy() *Policy {
	for i := range c.Update.Policies {
		if c.Update.Policies[i].IsDefault() {
			return &c.Update.Policies[i]
		}
	}
	c.Update.Policies = append(c.Update.Policies, DefaultPolicy())
	return &c.Update.Policies[len(c.Update.Policies)-1]
}

func (o *overlay) applySchedule() error {
	v, ok := o.env.get(envSchedule)
	if !ok {
		return nil
	}
	cron, interval, err := parseScheduleValue(v)
	if err != nil {
		return envError(envSchedule, v, err)
	}
	o.setSchedule(cron, interval)
	o.record(envSchedule, "schedule")
	return nil
}

func parseScheduleValue(v string) (cron string, interval time.Duration, err error) {
	if d, derr := ParseDuration(v); derr == nil {
		if d <= 0 {
			return "", 0, errors.New("an interval must be positive")
		}
		return "", d, nil
	}
	if _, cerr := ParseCron(v); cerr != nil {
		return "", 0, fmt.Errorf("neither an interval (6h, 1d) nor a cron spec (0 4 * * *): %w", cerr)
	}
	return v, 0, nil
}

func (o *overlay) setSchedule(cron string, interval time.Duration) {
	o.c.Schedule.Cron = cron
	o.c.Schedule.Interval = Duration(interval)
}

func (o *overlay) applyTimezone() error {
	v, ok := o.env.get(envZone)
	if !ok {
		return nil
	}
	if _, err := time.LoadLocation(v); err != nil {
		return envError(envZone, v, err)
	}
	o.c.Timezone = v
	o.record(envZone, "timezone")
	return nil
}

func (o *overlay) applyInstance() error {
	v, ok := o.env.get(envInstance)
	if !ok {
		return nil
	}
	if err := validateInstance(v); err != nil {
		return fmt.Errorf("%s: %w", envInstance, err)
	}
	o.c.Instance = v
	o.record(envInstance, "instance")
	return nil
}

func (o *overlay) applyStateDir() error {
	v, ok := o.env.get(envStateDir)
	if !ok {
		return nil
	}
	if !filepath.IsAbs(v) {
		return envError(envStateDir, v, errors.New("want an absolute path"))
	}
	o.c.State.Dir = v
	o.record(envStateDir, "state.dir")
	return nil
}

var (
	logLevels  = []string{"debug", "info", "warn", "warning", "error"}
	logFormats = []string{"json", "text"}
)

func (o *overlay) applyLog() error {
	if v, ok := o.env.get(envLogLevel); ok {
		l := strings.ToLower(v)
		if !slices.Contains(logLevels, l) {
			return envError(envLogLevel, v, errors.New("want debug, info, warn or error"))
		}
		o.c.Log.Level = l
		o.record(envLogLevel, "log.level")
	}
	if v, ok := o.env.get(envLogFormat); ok {
		f := strings.ToLower(v)
		if !slices.Contains(logFormats, f) {
			return envError(envLogFormat, v, errors.New("want json or text"))
		}
		o.c.Log.Format = f
		o.record(envLogFormat, "log.format")
	}
	return nil
}

func (o *overlay) applyHTTPPort() error {
	v, ok := o.env.get(envHTTPPort)
	if !ok {
		return nil
	}
	port, err := parsePort(v)
	if err != nil {
		return envError(envHTTPPort, v, err)
	}
	o.setHTTPPort(port)
	o.record(envHTTPPort, "http.port")
	return nil
}

func parsePort(v string) (int, error) {
	port, err := strconv.Atoi(v)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("want a port number from 1 to 65535")
	}
	return port, nil
}

func (o *overlay) setHTTPPort(port int) {
	h := &o.c.HTTP
	h.Port = port
	if h.Addr != "" {
		host, _, err := net.SplitHostPort(h.Addr)
		if err != nil {
			host = ""
		}
		h.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
}

func (o *overlay) applyToken(variable, key string, value, file *string) error {
	v, source, err := o.secretVar(variable)
	if err != nil {
		return err
	}
	if source == "" {
		return nil
	}
	*value, *file = v, ""
	o.record(source, key)
	return nil
}

func (o *overlay) applyNotifyURLs() error {
	v, source, err := o.secretVar(envNotifyURLs)
	if err != nil {
		return err
	}
	if source == "" {
		return nil
	}
	urls := splitURLs(v)
	if len(urls) == 0 {
		return fmt.Errorf("%s: no notification URL found", source)
	}
	for i, raw := range urls {
		if _, err := o.addNotifyURL(fmt.Sprintf("%s[%d]", source, i), raw); err != nil {
			return err
		}
	}
	o.record(source, "notify[]")
	return nil
}

func (o *overlay) addNotifyURL(label, raw string) ([]int, error) {
	var out []NotifierConfig
	var notes []string
	switch {
	case isServiceURL(raw):
		var err error
		if out, notes, err = convertServiceURL(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
	case webhookType(raw) != "":
		out = []NotifierConfig{{Type: webhookType(raw), URL: strings.TrimSpace(raw)}}
	default:
		return nil, fmt.Errorf("%s: want a notification service URL such as discord://… or ntfy://…, or a Discord, Slack or Teams workflow webhook URL (generic+https://… for any other webhook); supported are %s", label, notifySchemes)
	}
	for _, n := range notes {
		o.warn("%s: %s", label, n)
	}
	var added []int
	for _, n := range out {
		n.Name = o.c.uniqueNotifierName(n.Type)
		o.c.Notify = append(o.c.Notify, n)
		o.c.notifyOrigin = append(o.c.notifyOrigin, label)
		added = append(added, len(o.c.Notify)-1)
	}
	return added, nil
}

func (c *Config) uniqueNotifierName(base string) string {
	used := map[string]bool{}
	for _, n := range c.Notify {
		name := n.Name
		if name == "" {
			name = n.Type
		}
		used[name] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		if name := fmt.Sprintf("%s-%d", base, i); !used[name] {
			return name
		}
	}
}

var nextURLRE = regexp.MustCompile(`,[A-Za-z][A-Za-z0-9+.-]*://`)

func splitURLs(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			lines = append(lines, line)
		}
	}
	var out []string
	for _, field := range strings.Fields(strings.Join(lines, "\n")) {
		for {
			loc := nextURLRE.FindStringIndex(field)
			if loc == nil {
				break
			}
			if part := strings.Trim(field[:loc[0]], ","); part != "" {
				out = append(out, part)
			}
			field = field[loc[0]+1:]
		}
		if part := strings.Trim(field, ","); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (o *overlay) applyNotifyEvents() error {
	v, ok := o.env.get(envNotifyEvents)
	if !ok {
		return nil
	}
	events, err := parseEvents(v)
	if err != nil {
		return envError(envNotifyEvents, v, err)
	}
	for i := range o.c.Notify {
		if len(o.c.Notify[i].Events) == 0 || o.watchtowerNotify[i] {
			o.c.Notify[i].Events = append([]string(nil), events...)
		}
	}
	o.record(envNotifyEvents, "notify[].events")
	return nil
}

func parseEvents(v string) ([]string, error) {
	events := strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	if len(events) == 0 {
		return nil, errors.New("no events listed")
	}
	for _, e := range events {
		if e != "*" && !slices.Contains(notifyEvents, e) {
			return nil, errors.New(unknownValue("event", e, append(slices.Clone(notifyEvents), "*")))
		}
	}
	return events, nil
}

func (o *overlay) applyMode() error {
	v, ok := o.env.get(envMode)
	if !ok {
		return nil
	}
	m := strings.ToLower(v)
	if !ValidMode(m) {
		return envError(envMode, v, fmt.Errorf("unknown mode (want %s, %s, %s, %s or %s)", ModeTagFollow, ModePatch, ModeMinor, ModeMajor, ModeNone))
	}
	o.c.defaultPolicy().Mode = m
	o.record(envMode, "update.policies[default].mode")
	return nil
}

func (o *overlay) checkUpdateAPI() error {
	if o.updateAPIFrom != "" && o.c.HTTP.UpdateAPI && o.c.HTTP.Token == "" {
		return fmt.Errorf("%s: the update API requires a token (%s, %s%s or http.token) — without one anyone on the network could restart containers", o.updateAPIFrom, envHTTPAuth, envHTTPAuth, fileSuffix)
	}
	return nil
}

func (o *overlay) warnMisspeltDockpaumEnv() {
	for _, name := range o.env.names("DOCKPAUM_") {
		if _, set := o.env.get(name); !set || slices.Contains(knownDockpaumEnv, name) {
			continue
		}
		if best := closest(name, knownDockpaumEnv); best != "" {
			o.warn("%s is not a dockpaum setting and is ignored — did you mean %s?", name, best)
		}
	}
}

func closest(name string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
