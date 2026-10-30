package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

func (c *Config) Warnings() []string {
	w := append([]string(nil), c.loadWarnings...)
	add := func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }

	plainHTTP := func(path, raw string) {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			add("%s: plain http:// to %s sends tokens unencrypted", path, u.Hostname())
		}
	}
	if c.Update.ComposeGroup && c.Update.Strategy == StrategyParallel {
		add("update.strategy: parallel has no effect with update.compose_group: true; Compose projects are updated one container at a time")
	}
	for i, n := range c.Notify {
		base := c.notifyLabel(i)
		plainHTTP(base+".url", n.URL)
		if n.Type == "teams" && retiredTeamsConnector(n.URL) {
			add("%s (teams).url: Office 365 connector webhooks were retired by Microsoft and no longer deliver; create a Teams workflow with \"When a Teams webhook request is received\" and use its URL", base)
		}
		if n.SMTP != nil && !n.SMTP.StartTLS && n.SMTP.Port != smtpsPort {
			add("%s.smtp: starttls is off — if the server does not offer STARTTLS, mail travels unencrypted (and authenticated delivery is refused)", base)
		}
	}
	if a := c.Update.Approval; a != nil {
		if a.URL == "" {
			add("update.approval: no url — the gate approves every update")
		}
		switch {
		case a.FailOpen && a.ApprovesOnFailure():
			add("update.approval.fail_open has no effect with approve_on: failure: an endpoint that cannot be reached approves the update anyway")
		case a.FailOpen:
			add("update.approval.fail_open: updates go through while the approver is unreachable")
		}
		plainHTTP("update.approval.url", a.URL)
		if a.Signing != nil && a.URL != "" {
			if _, err := a.Signing.Secret(); err != nil {
				add("%v — approval requests cannot be signed, so every update is denied until it can be read", err)
			}
		}
	}
	for i, h := range c.Hosts {
		if strings.HasPrefix(h.Endpoint, "tcp://") && h.TLS == nil {
			add("hosts[%d] (%s): tcp:// without TLS exposes root-equivalent Docker access to the network", i, h.Name)
		}
		if h.TLS != nil && !h.TLS.ShouldVerify() {
			add("hosts[%d] (%s): TLS certificate verification is off", i, h.Name)
		}
	}
	if !c.Update.Rollback() {
		add("update.rollback_on_failure is false: a replacement that fails to start leaves the old container stopped")
	}
	for i, p := range c.Update.Policies {
		label := fmt.Sprintf("update.policies[%d] (id=%q)", i, p.ID)
		if p.HealthTimeout == 0 && p.Mode != ModeNone && !p.MonitorOnly {
			add("%s.health_timeout is 0s: the health gate is off, so a replacement that starts and then crashes or turns unhealthy is kept (remove the key for the %s default)", label, formatDuration(DefaultHealthTimeout))
		}
		if p.MonitorOnly && p.Mode == ModeNone {
			add("%s.monitor_only has no effect with mode none: mode none never checks for updates, so there is nothing to report — use mode tag-follow (or patch, minor, major) to monitor", label)
		}
		if !p.Tags.IsZero() && !IsSemverMode(p.Mode) {
			add("%s.tags has no effect with mode %s: only patch, minor and major choose among version tags", label, p.Mode)
		}
		if b, ok := BuiltinPolicy(p.ID, DefaultPolicy()); ok && !behavesLike(p, b) {
			add("%s replaces the built-in policy %s with different behavior (mode %s, monitor_only %t): containers labelled dockpaum.policy=%s get this policy — choose another id unless that is intended", label, b.ID, p.Mode, p.MonitorOnly, b.ID)
		}
	}
	if c.Update.Strategy == StrategyRolling && c.Update.Concurrency > 1 {
		add("update.concurrency is ignored with strategy rolling")
	}
	if c.Hooks.LabelHooks {
		add("hooks.label_hooks: commands from container labels run inside those containers — only enable where you control every label")
	}
	if c.DryRun && (len(c.Jobs) > 0 || len(c.Hooks.PreRun) > 0 || len(c.Hooks.PostRun) > 0) {
		add("dry_run is on: jobs and the pre_run/post_run hooks are skipped until dry_run is false")
	}
	if c.Schedule.Cron == "" && c.Schedule.Interval > 0 && c.Schedule.Interval.Std() < time.Minute {
		add("schedule.interval: a check every %s queries every registry that often and soon hits rate limits (Docker Hub limits pulls) — use %s (the default) or longer", formatDuration(c.Schedule.Interval.Std()), formatDuration(DefaultInterval))
	}
	for i, h := range c.Hosts {
		ep := strings.ToLower(strings.TrimSpace(h.Endpoint))
		if ep != "" && !strings.HasPrefix(ep, "unix://") && !strings.HasPrefix(ep, "tcp://") && !strings.HasPrefix(ep, "npipe://") {
			add("hosts[%d] (%s).endpoint: %q is not a Docker endpoint dockpaum knows (unix://, tcp:// or npipe://); connecting will likely fail", i, h.Name, h.Endpoint)
		}
	}
	if c.HTTP.UpdateAPI && !c.HTTP.IsEnabled() {
		add("%s has no effect: http.enabled is false, so nothing serves the API", c.setting("http.update_api"))
	}
	if c.HTTP.WatchtowerCompat && !c.HTTP.IsEnabled() {
		add("%s has no effect: http.enabled is false, so nothing serves /v1/update or /v1/metrics", c.setting("http.watchtower_compat"))
	}
	if !c.State.IsEnabled() && c.State.Dir != "" {
		add("%s has no effect: state.enabled is false, so the state is kept in memory only", c.setting("state.dir"))
	}
	if !c.State.IsEnabled() {
		for i, p := range c.Update.Policies {
			if p.MinImageAgeSource == AgeSourceFirstSeen && p.MinImageAge > 0 && p.Mode != ModeNone {
				add("update.policies[%d] (id=%q).min_image_age_source: first_seen needs the durable state, but state.enabled is false — min_image_age is measured from the image's creation time instead (as with auto)", i, p.ID)
			}
		}
	}
	if dc := strings.TrimSpace(c.DockerConfig); dc != "" && !filepath.IsAbs(dc) {
		add("%s: %q is a relative path, resolved from dockpaum's working directory — use an absolute path", c.setting("docker_config"), c.DockerConfig)
	}
	for i, r := range c.Registry {
		if r.Username == "" && r.Password == "" && r.PasswordFile == "" {
			add("registry[%d] (%s): no username, password or password_file — the entry has no effect", i, r.Server)
		}
	}
	w = append(w, c.timingWarnings()...)
	for i := range w {
		w[i] = c.withLine(w[i])
	}
	return w
}

func behavesLike(p, b Policy) bool {
	mode := p.Mode
	if mode == ModeDigest {
		mode = ModeTagFollow
	}
	return mode == b.Mode && p.MonitorOnly == b.MonitorOnly
}

func (c *Config) Notes() []string {
	var n []string
	add := func(format string, args ...any) { n = append(n, fmt.Sprintf(format, args...)) }
	if iv := c.Schedule.Interval.Std(); c.Schedule.Cron == "" && iv >= time.Minute && iv < DefaultInterval {
		add("schedule.interval: a check every %s is frequent; registries such as Docker Hub rate-limit pulls (the default is %s)", formatDuration(iv), formatDuration(DefaultInterval))
	}
	if c.HTTP.IsEnabled() && c.HTTP.Token == "" && c.HTTP.ReadToken == "" {
		open := "/internal-api/ health, ready, busy, version, metrics, status, containers and summary are"
		if c.HTTP.WatchtowerCompat {
			open = "/internal-api/ health, ready, busy, version, metrics, status, containers and summary, and /v1/metrics, are"
		}
		add("http.token: not set, so %s served without authentication on %s", open, c.HTTP.ListenAddr())
	}
	if c.HTTP.IsEnabled() && c.HTTP.WatchtowerCompat && !c.HTTP.UpdateAPI {
		add("%s: only /v1/metrics is served; /v1/update also needs http.update_api", c.setting("http.watchtower_compat"))
	}
	for i, n := range c.Notify {
		if u, err := url.Parse(n.URL); err == nil && n.Type == "ntfy" && n.Token == "" && strings.EqualFold(u.Hostname(), "ntfy.sh") {
			add("%s (ntfy).url: anyone who knows a topic on ntfy.sh can read it; use a hard-to-guess topic name or a token", c.notifyLabel(i))
		}
	}
	for i, p := range c.Update.Policies {
		label := fmt.Sprintf("update.policies[%d] (id=%q)", i, p.ID)
		if p.Mode == ModeDigest {
			add("%s.mode: digest is another name for tag-follow", label)
		}
		if p.Schedule != "" && len(c.Schedule.Windows) > 0 {
			add("%s.schedule: schedule.windows also gate this policy's runs", label)
		}
	}
	for i := range n {
		n[i] = c.withLine(n[i])
	}
	return n
}

func (c *Config) setting(key string) string {
	for i := len(c.envOverrides) - 1; i >= 0; i-- {
		if e := c.envOverrides[i]; e.Key == key {
			return fmt.Sprintf("%s (%s)", e.Var, key)
		}
	}
	return key
}

func (c *Config) withLine(msg string) string {
	if strings.HasPrefix(msg, "line ") {
		return msg
	}
	if path := messagePath(msg); path != "" {
		if s, ok := c.lines.lookup(path); ok {
			return fmt.Sprintf("line %d: %s", s.start, msg)
		}
	}
	return msg
}

var deferredRefRE = regexp.MustCompile(`\$\{(DOCKPAUM_[A-Za-z0-9_]*)\}`)

func (c *Config) validateNoDeferredRefs() error {
	check := func(path, v string) error {
		m := deferredRefRE.FindStringSubmatch(strings.ReplaceAll(v, "$$", ""))
		if m == nil {
			return nil
		}
		msg := fmt.Sprintf("%s: ${%s} stays literal here — ${DOCKPAUM_*} references are kept for hooks and jobs and expanded only there", path, m[1])
		if slices.Contains(knownDockpaumEnv, m[1]) {
			return fmt.Errorf("%s; %s is read from the environment by itself and overrides the config file, so remove the reference", msg, m[1])
		}
		return fmt.Errorf("%s; reference a variable without the DOCKPAUM_ prefix", msg)
	}
	fields := map[string]string{
		"http.token": c.HTTP.Token, "http.read_token": c.HTTP.ReadToken,
		"instance": c.Instance, "state.dir": c.State.Dir,
	}
	for i, r := range c.Registry {
		fields[fmt.Sprintf("registry[%d].username", i)] = r.Username
		fields[fmt.Sprintf("registry[%d].password", i)] = r.Password
	}
	for i, n := range c.Notify {
		base := fmt.Sprintf("notify[%d]", i)
		fields[base+".url"] = n.URL
		fields[base+".token"] = n.Token
		fields[base+".user"] = n.User
		if n.Type == "apprise" {
			for j, t := range n.To {
				fields[fmt.Sprintf("%s.to[%d]", base, j)] = t
			}
		}
		for k, v := range n.Headers {
			fields[base+".headers."+k] = v
		}
		if n.SMTP != nil {
			fields[base+".smtp.username"] = n.SMTP.Username
			fields[base+".smtp.password"] = n.SMTP.Password
		}
	}
	if a := c.Update.Approval; a != nil {
		fields["update.approval.url"] = a.URL
		for k, v := range a.Headers {
			fields["update.approval.headers."+k] = v
		}
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := check(k, fields[k]); err != nil {
			return err
		}
	}
	return nil
}
