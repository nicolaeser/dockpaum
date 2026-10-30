package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var watchtowerFileVars = []string{
	"WATCHTOWER_HTTP_API_TOKEN",
	"WATCHTOWER_NOTIFICATION_URL",
	"WATCHTOWER_NOTIFICATION_EMAIL_SERVER_PASSWORD",
	"WATCHTOWER_NOTIFICATION_GOTIFY_TOKEN",
	"WATCHTOWER_NOTIFICATION_SLACK_HOOK_URL",
	"WATCHTOWER_NOTIFICATION_MSTEAMS_HOOK_URL",
	"REPO_PASS",
}

var watchtowerHints = map[string]string{
	"WATCHTOWER_REVIVE_STOPPED":                            "stopped containers stay stopped after an update",
	"WATCHTOWER_NO_RESTART":                                "dockpaum always recreates and restarts an updated container",
	"WATCHTOWER_NO_PULL":                                   "there is no global switch; label the containers dockpaum.no-pull=true",
	"WATCHTOWER_REMOVE_VOLUMES":                            "anonymous volumes of replaced containers are kept",
	"WATCHTOWER_LABEL_TAKE_PRECEDENCE":                     "container labels always take precedence over the configuration",
	"WATCHTOWER_WARN_ON_HEAD_FAILURE":                      "registry check failures are always reported (check-failed)",
	"WATCHTOWER_HEALTH_CHECK":                              "the image has its own HEALTHCHECK (dockpaum --healthcheck)",
	"WATCHTOWER_PORCELAIN":                                 "logs are JSON or text (DOCKPAUM_LOG_FORMAT)",
	"WATCHTOWER_NOTIFICATION_TEMPLATE":                     "dockpaum formats its own messages",
	"WATCHTOWER_NOTIFICATIONS_HOSTNAME":                    "messages name the Docker host",
	"WATCHTOWER_NOTIFICATION_TITLE_TAG":                    "dockpaum formats its own messages",
	"WATCHTOWER_NOTIFICATION_SKIP_TITLE":                   "dockpaum formats its own messages",
	"WATCHTOWER_NOTIFICATIONS_DELAY":                       "notifications are sent when the event happens",
	"WATCHTOWER_NOTIFICATION_EMAIL_DELAY":                  "notifications are sent when the event happens",
	"WATCHTOWER_NOTIFICATION_EMAIL_SUBJECTTAG":             "dockpaum formats its own messages",
	"WATCHTOWER_NOTIFICATION_SLACK_IDENTIFIER":             "the webhook's own name is used",
	"WATCHTOWER_NOTIFICATION_SLACK_ICON_EMOJI":             "the webhook's own icon is used",
	"WATCHTOWER_NOTIFICATION_SLACK_ICON_URL":               "the webhook's own icon is used",
	"WATCHTOWER_NOTIFICATION_MSTEAMS_USE_LOG_DATA":         "dockpaum formats its own messages",
	"WATCHTOWER_NOTIFICATION_LOG_STDOUT":                   "dockpaum logs every event",
	"WATCHTOWER_NOTIFICATION_EMAIL_SERVER_TLS_SKIP_VERIFY": "the mail server certificate is always verified",
	"WATCHTOWER_NOTIFICATION_GOTIFY_TLS_SKIP_VERIFY":       "the Gotify server certificate is always verified",
}

type watchtowerVars struct {
	o      *overlay
	seen   map[string]bool
	legacy map[string]bool
}

var watchtowerLegacyPrefixes = map[string]string{
	"WATCHTOWER_NOTIFICATION_EMAIL_":   "email",
	"WATCHTOWER_NOTIFICATION_SLACK_":   "slack",
	"WATCHTOWER_NOTIFICATION_MSTEAMS_": "msteams",
	"WATCHTOWER_NOTIFICATION_GOTIFY_":  "gotify",
}

func (w *watchtowerVars) legacyTypeOff(name string) string {
	for prefix, typ := range watchtowerLegacyPrefixes {
		if strings.HasPrefix(name, prefix) && !w.legacy[typ] {
			return typ
		}
	}
	return ""
}

func (w *watchtowerVars) get(name string) (string, bool) {
	w.seen[name] = true
	return w.o.env.get(name)
}

func (w *watchtowerVars) value(name string) (string, bool, error) {
	v, ok := w.get(name)
	if !ok || !slices.Contains(watchtowerFileVars, name) {
		return v, ok, nil
	}
	if strings.HasPrefix(v, "/") && !strings.Contains(v, "://") {
		s, err := readSecretFile(v)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", name, err)
		}
		return s, true, nil
	}
	return v, true, nil
}

func (w *watchtowerVars) flag(name string) (on, ok bool, err error) {
	v, ok := w.get(name)
	if !ok {
		return false, false, nil
	}
	b, err := parseBool(v)
	if err != nil {
		return false, false, envError(name, v, err)
	}
	return b, true, nil
}

func (w *watchtowerVars) on(name string) (bool, error) {
	b, _, err := w.flag(name)
	return b, err
}

func (o *overlay) applyWatchtower() error {
	w := &watchtowerVars{o: o, seen: map[string]bool{}, legacy: map[string]bool{}}
	for _, step := range []func(*watchtowerVars) error{
		applyWatchtowerSchedule,
		applyWatchtowerSelection,
		applyWatchtowerUpdate,
		applyWatchtowerHTTP,
		applyWatchtowerLog,
		applyWatchtowerRegistry,
		applyWatchtowerNotify,
	} {
		if err := step(w); err != nil {
			return err
		}
	}
	if !o.c.DryRun {
		o.c.DryRun = true
		o.watchtowerDryRun = true
		o.record(envWatchtowerEnv, "dry_run")
	}
	for _, name := range o.env.names("WATCHTOWER_") {
		if w.seen[name] {
			continue
		}
		v, ok := o.env.get(name)
		if !ok {
			continue
		}
		if b, err := parseBool(v); err == nil && !b {
			continue
		}
		if typ := w.legacyTypeOff(name); typ != "" {
			o.warn("%s is ignored: WATCHTOWER_NOTIFICATIONS does not list %s (Watchtower ignores it too)", name, typ)
		} else if hint, known := watchtowerHints[name]; known {
			o.warn("%s is not supported and is ignored: %s", name, hint)
		} else {
			o.warn("%s is set but has no dockpaum equivalent; it is ignored", name)
		}
	}
	return nil
}

const watchtowerDefaultInterval = 24 * time.Hour

func (w *watchtowerVars) anySet() bool {
	for _, name := range w.o.env.names("WATCHTOWER_") {
		if _, ok := w.o.env.get(name); ok {
			return true
		}
	}
	return false
}

func applyWatchtowerSchedule(w *watchtowerVars) error {
	o := w.o
	cron, hasCron := w.get("WATCHTOWER_SCHEDULE")
	interval, hasInterval := w.get("WATCHTOWER_POLL_INTERVAL")
	switch {
	case hasCron && hasInterval:
		return errors.New("WATCHTOWER_SCHEDULE and WATCHTOWER_POLL_INTERVAL are both set; Watchtower refuses that too — keep one")
	case hasCron:
		if _, err := ParseCron(cron); err != nil {
			return envError("WATCHTOWER_SCHEDULE", cron, err)
		}
		o.setSchedule(cron, 0)
		o.record("WATCHTOWER_SCHEDULE", "schedule")
	case hasInterval:
		d, err := ParseDuration(interval)
		if err != nil {
			return envError("WATCHTOWER_POLL_INTERVAL", interval, err)
		}
		if d <= 0 {
			return envError("WATCHTOWER_POLL_INTERVAL", interval, errors.New("must be positive"))
		}
		o.setSchedule("", d)
		o.record("WATCHTOWER_POLL_INTERVAL", "schedule")
	case o.c.Schedule.Cron == "" && o.c.Schedule.Interval == 0 && !o.env.has(envSchedule) && w.anySet():
		o.setSchedule("", watchtowerDefaultInterval)
		o.warn("WATCHTOWER_* variables are set without WATCHTOWER_SCHEDULE or WATCHTOWER_POLL_INTERVAL: checking every 24h as Watchtower did; set %s (or schedule: in the config) to change it", envSchedule)
	}
	if on, err := w.on("WATCHTOWER_UPDATE_ON_START"); err != nil {
		return err
	} else if on {
		o.c.Schedule.RunOnStart = &on
		o.record("WATCHTOWER_UPDATE_ON_START", "schedule.run_on_start")
	}
	if on, err := w.on("WATCHTOWER_RUN_ONCE"); err != nil {
		return err
	} else if on {
		o.c.runOnce = true
		o.record("WATCHTOWER_RUN_ONCE", "--once")
	}
	return nil
}

func applyWatchtowerSelection(w *watchtowerVars) error {
	o, c := w.o, w.o.c
	if on, set, err := w.flag("WATCHTOWER_LABEL_ENABLE"); err != nil {
		return err
	} else if set {
		c.Filters.LabelEnable = on
		o.record("WATCHTOWER_LABEL_ENABLE", "filters.label_enable")
	}
	if on, set, err := w.flag("WATCHTOWER_INCLUDE_STOPPED"); err != nil {
		return err
	} else if set {
		c.Filters.IncludeStopped = on
		o.record("WATCHTOWER_INCLUDE_STOPPED", "filters.include_stopped")
	}
	if on, err := w.on("WATCHTOWER_INCLUDE_RESTARTING"); err != nil {
		return err
	} else if on && !c.Filters.IncludeStopped {
		o.warn("WATCHTOWER_INCLUDE_RESTARTING is not supported on its own: filters.include_stopped (%s=true) covers restarting containers together with stopped ones", envIncludeStopped)
	}
	if v, ok := w.get("WATCHTOWER_SCOPE"); ok {
		c.Filters.Scope = v
		o.record("WATCHTOWER_SCOPE", "filters.scope")
	}
	if v, ok := w.get("WATCHTOWER_DISABLE_CONTAINERS"); ok {
		for _, name := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			name = strings.TrimPrefix(name, "/")
			if name != "" && !slices.Contains(c.Filters.ExcludeNames, name) {
				c.Filters.ExcludeNames = append(c.Filters.ExcludeNames, name)
			}
		}
		o.record("WATCHTOWER_DISABLE_CONTAINERS", "filters.exclude_names[]")
	}
	return nil
}

var watchtowerMonthRE = regexp.MustCompile(`(\d+(?:\.\d+)?)M`)

func applyWatchtowerUpdate(w *watchtowerVars) error {
	o, c := w.o, w.o.c
	if on, set, err := w.flag("WATCHTOWER_CLEANUP"); err != nil {
		return err
	} else if set {
		c.Update.CleanupImages = on
		o.record("WATCHTOWER_CLEANUP", "update.cleanup_images")
	}
	if on, err := w.on("WATCHTOWER_MONITOR_ONLY"); err != nil {
		return err
	} else if on {
		c.DryRun = true
		o.record("WATCHTOWER_MONITOR_ONLY", "dry_run")
	}
	if on, err := w.on("WATCHTOWER_ROLLING_RESTART"); err != nil {
		return err
	} else if on {
		c.Update.Strategy = StrategyRolling
		o.record("WATCHTOWER_ROLLING_RESTART", "update.strategy")
	}
	if v, ok := w.get("WATCHTOWER_TIMEOUT"); ok {
		d, err := ParseDuration(v)
		if err != nil || d < 0 {
			return envError("WATCHTOWER_TIMEOUT", v, errors.New("want a non-negative duration such as 30s"))
		}
		c.Update.StopTimeout = Duration(d)
		o.record("WATCHTOWER_TIMEOUT", "update.stop_timeout")
	}
	if v, ok := w.get("WATCHTOWER_COOLDOWN_DELAY"); ok {
		d, err := ParseDuration(watchtowerMonthRE.ReplaceAllStringFunc(v, func(m string) string {
			n, _ := strconv.ParseFloat(strings.TrimSuffix(m, "M"), 64)
			return strconv.FormatFloat(n*30, 'f', -1, 64) + "d"
		}))
		if err != nil || d < 0 {
			return envError("WATCHTOWER_COOLDOWN_DELAY", v, errors.New("want a non-negative duration such as 3d"))
		}
		p := c.defaultPolicy()
		p.MinImageAge, p.minImageAgeSet = Duration(d), true
		o.record("WATCHTOWER_COOLDOWN_DELAY", "update.policies[default].min_image_age")
	}
	if on, err := w.on("WATCHTOWER_LIFECYCLE_HOOKS"); err != nil {
		return err
	} else if on {
		c.Hooks.LabelHooks = true
		o.record("WATCHTOWER_LIFECYCLE_HOOKS", "hooks.label_hooks")
	}
	return nil
}

func applyWatchtowerHTTP(w *watchtowerVars) error {
	o, c := w.o, w.o.c
	token, hasToken, err := w.value("WATCHTOWER_HTTP_API_TOKEN")
	if err != nil {
		return err
	}
	if hasToken {
		c.HTTP.Token, c.HTTP.TokenFile = token, ""
		o.record("WATCHTOWER_HTTP_API_TOKEN", "http.token")
	}
	update, err := w.on("WATCHTOWER_HTTP_API_UPDATE")
	if err != nil {
		return err
	}
	if update {
		c.HTTP.UpdateAPI = true
		o.updateAPIFrom = "WATCHTOWER_HTTP_API_UPDATE"
		o.record("WATCHTOWER_HTTP_API_UPDATE", "http.update_api")
	}
	polls, err := w.on("WATCHTOWER_HTTP_API_PERIODIC_POLLS")
	if err != nil {
		return err
	}
	if update && !polls {
		o.warn("WATCHTOWER_HTTP_API_UPDATE without WATCHTOWER_HTTP_API_PERIODIC_POLLS: Watchtower then updates only on API calls, while dockpaum keeps running its regular schedule as well")
	}
	metrics, err := w.on("WATCHTOWER_HTTP_API_METRICS")
	if err != nil {
		return err
	}
	if metrics || update {
		enabled := true
		c.HTTP.Enabled = &enabled
		c.HTTP.WatchtowerCompat = true
		variable := "WATCHTOWER_HTTP_API_UPDATE"
		if metrics {
			variable = "WATCHTOWER_HTTP_API_METRICS"
		}
		o.record(variable, "http.enabled")
		o.record(variable, "http.watchtower_compat")
	}
	host, hasHost := w.get("WATCHTOWER_HTTP_API_HOST")
	portValue, hasPort := w.get("WATCHTOWER_HTTP_API_PORT")
	port := 8080
	if hasPort {
		if port, err = parsePort(portValue); err != nil {
			return envError("WATCHTOWER_HTTP_API_PORT", portValue, err)
		}
	}
	if hasHost {
		if net.ParseIP(host) == nil && strings.ContainsAny(host, ":/ ") {
			return envError("WATCHTOWER_HTTP_API_HOST", host, errors.New("want an IP address or host name"))
		}
		if !hasPort {
			port = c.HTTP.listenPort()
		}
		c.HTTP.Addr = net.JoinHostPort(host, strconv.Itoa(port))
		o.record("WATCHTOWER_HTTP_API_HOST", "http.addr")
	} else if hasPort {
		o.setHTTPPort(port)
		o.record("WATCHTOWER_HTTP_API_PORT", "http.port")
	}
	return nil
}

var watchtowerLogLevels = map[string]string{
	"panic": "error", "fatal": "error", "error": "error",
	"warn": "warn", "warning": "warn", "info": "info", "debug": "debug", "trace": "debug",
}

func applyWatchtowerLog(w *watchtowerVars) error {
	o, c := w.o, w.o.c
	trace, err := w.on("WATCHTOWER_TRACE")
	if err != nil {
		return err
	}
	debug, err := w.on("WATCHTOWER_DEBUG")
	if err != nil {
		return err
	}
	level, hasLevel := w.get("WATCHTOWER_LOG_LEVEL")
	switch {
	case trace:
		c.Log.Level = "debug"
		o.record("WATCHTOWER_TRACE", "log.level")
	case debug:
		c.Log.Level = "debug"
		o.record("WATCHTOWER_DEBUG", "log.level")
	case hasLevel:
		l, ok := watchtowerLogLevels[strings.ToLower(level)]
		if !ok {
			return envError("WATCHTOWER_LOG_LEVEL", level, errors.New("want panic, fatal, error, warn, info, debug or trace"))
		}
		c.Log.Level = l
		o.record("WATCHTOWER_LOG_LEVEL", "log.level")
	}
	if v, ok := w.get("WATCHTOWER_LOG_FORMAT"); ok {
		switch strings.ToLower(v) {
		case "json":
			c.Log.Format = "json"
		case "auto", "logfmt", "pretty":
			c.Log.Format = "text"
		default:
			return envError("WATCHTOWER_LOG_FORMAT", v, errors.New("want Auto, LogFmt, Pretty or JSON"))
		}
		o.record("WATCHTOWER_LOG_FORMAT", "log.format")
	}
	return nil
}

func applyWatchtowerRegistry(w *watchtowerVars) error {
	o, c := w.o, w.o.c
	user, hasUser := w.get("REPO_USER")
	pass, hasPass, err := w.value("REPO_PASS")
	if err != nil {
		return err
	}
	if !hasUser && !hasPass {
		return nil
	}
	if !hasUser || !hasPass {
		return errors.New("REPO_USER and REPO_PASS must be set together")
	}
	for i, r := range c.Registry {
		if host, path := r.Scope(); path == "" && isDockerHubHost(host) {
			o.warn("REPO_USER/REPO_PASS replace the Docker Hub credentials of registry[%d] (%s) from the config file", i, r.Server)
		}
	}
	c.Registry = append([]RegistryAuth{{Server: "docker.io", Username: user, Password: pass}}, c.Registry...)
	o.record("REPO_USER", "registry[]")
	o.warn("REPO_USER/REPO_PASS apply to Docker Hub (docker.io) only, while Watchtower sent them to every registry; add registry: entries (or docker_config) for other registries")
	return nil
}

func isDockerHubHost(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

func (o *overlay) warnWatchtowerIgnored(switchedOff bool) {
	var ignored []string
	for _, name := range append(o.env.names("WATCHTOWER_"), "REPO_USER", "REPO_PASS") {
		if _, ok := o.env.get(name); ok {
			ignored = append(ignored, name)
		}
	}
	switch {
	case len(ignored) == 0:
	case switchedOff:
		o.warn("%s=false: ignoring %s", envWatchtowerEnv, strings.Join(ignored, ", "))
	default:
		o.warn("ignoring %s: set %s=true to read the Watchtower settings (dockpaum then keeps dry_run on until %s=false), or remove them", strings.Join(ignored, ", "), envWatchtowerEnv, envDryRun)
	}
}

func (o *overlay) warnWatchtowerReplacesFile() {
	for _, e := range o.c.envOverrides {
		if !strings.HasPrefix(e.Var, "WATCHTOWER_") || strings.HasSuffix(e.Key, "[]") {
			continue
		}
		if keys := o.fileKeys(e.Key); len(keys) > 0 {
			o.warn("%s replaces %s from the config file", e.Var, strings.Join(keys, " and "))
		}
	}
}

func (o *overlay) fileKeys(key string) []string {
	var candidates []string
	switch key {
	case "schedule":
		candidates = []string{"schedule.cron", "schedule.interval"}
	case "http.token":
		candidates = []string{"http.token", "http.token_file"}
	case "http.addr", "http.port":
		candidates = []string{"http.addr", "http.port"}
	case "update.policies[default].min_image_age":
		for i, p := range o.c.Update.Policies {
			if p.IsDefault() {
				candidates = []string{fmt.Sprintf("update.policies[%d].min_image_age", i)}
				break
			}
		}
	default:
		candidates = []string{key}
	}
	var out []string
	for _, k := range candidates {
		if _, ok := o.c.lines[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

func (o *overlay) applyDockerEnv() error {
	o.applyDockerConfigDir()
	c := o.c
	var envHosts []string
	for i, h := range c.Hosts {
		if strings.TrimSpace(h.Endpoint) == "" {
			name := fmt.Sprintf("hosts[%d]", i)
			if h.Name != "" {
				name += " (" + h.Name + ")"
			}
			envHosts = append(envHosts, name)
		}
	}
	usesEnv := len(c.Hosts) == 0 || len(envHosts) > 0
	host, hasHost := o.env.get("DOCKER_HOST")
	if !usesEnv {
		if hasHost {
			o.warn("DOCKER_HOST is ignored: every configured host sets its own endpoint")
		}
		return nil
	}
	if hasHost && strings.HasPrefix(strings.ToLower(host), "ssh://") {
		if len(envHosts) == len(c.Hosts) {
			return errors.New("DOCKER_HOST: ssh:// is not supported; use a local socket (unix://) or tcp:// with TLS")
		}
		o.warn("DOCKER_HOST: ssh:// is not supported, so %s cannot connect; give it an endpoint (unix://, or tcp:// with TLS) or unset DOCKER_HOST", strings.Join(envHosts, " and "))
		return nil
	}
	_, hasCerts := o.env.get("DOCKER_CERT_PATH")
	_, hasVerify := o.env.get("DOCKER_TLS_VERIFY")
	if hasHost && strings.HasPrefix(strings.ToLower(host), "tcp://") && !hasCerts && !isLoopbackEndpoint(host) {
		o.warn("DOCKER_HOST: tcp:// without TLS exposes root-equivalent Docker access to the network (TLS needs DOCKER_CERT_PATH with ca.pem, cert.pem and key.pem, and DOCKER_TLS_VERIFY=1)")
	}
	if hasCerts && !hasVerify {
		o.warn("DOCKER_CERT_PATH without DOCKER_TLS_VERIFY: TLS certificate verification of the Docker host is off")
	}
	if v, ok := o.env.get("DOCKER_API_VERSION"); ok {
		o.warn("DOCKER_API_VERSION=%s pins the Docker API version and turns off version negotiation; remove it unless a daemon requires it", v)
	}
	return nil
}

func isLoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && isLoopbackHost(u.Hostname())
}

func (o *overlay) applyDockerConfigDir() {
	dir, ok := o.env.get("DOCKER_CONFIG")
	if !ok {
		return
	}
	if o.c.DockerConfig != "" {
		o.warn("DOCKER_CONFIG is ignored: docker_config is set")
		return
	}
	path := filepath.Join(dir, "config.json")
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		o.warn("DOCKER_CONFIG=%s: there is no config.json in it, so no registry credentials are read from it", dir)
		return
	}
	o.c.DockerConfig = path
	o.record("DOCKER_CONFIG", "docker_config")
}

func (o *overlay) watchtowerObserveNote() {
	if !o.c.DryRun || o.env.has(envDryRun) {
		return
	}
	if on, err := parseBool(o.env["WATCHTOWER_MONITOR_ONLY"]); err == nil && on {
		return
	}
	overridden := ""
	if _, inFile := o.c.lines["dry_run"]; o.watchtowerDryRun && inFile {
		overridden = " (the config file's dry_run: false is overridden until then)"
	}
	o.warn("%s=true keeps dry_run on: dockpaum reports what it would update and changes nothing — set %s=false to apply updates%s", envWatchtowerEnv, envDryRun, overridden)
}
