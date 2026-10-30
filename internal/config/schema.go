package config

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
)

const SchemaURL = "https://raw.githubusercontent.com/nicolaeser/dockpaum/main/dockpaum.schema.json"

const durationPattern = `^\s*[+-]?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h|d|D|w|W))+\s*$|^\s*\d+\s*$`

const envRefPattern = `(?:^|[^$])(?:\$\$)*\$(?:\{[A-Za-z_][A-Za-z0-9_]*(?::?[-?+][^}]*)?\}|[A-Za-z_][A-Za-z0-9_]*)`

var yaml11Bools = []any{"y", "Y", "yes", "Yes", "YES", "on", "On", "ON", "n", "N", "no", "No", "NO", "off", "Off", "OFF"}

func JSONSchema() ([]byte, error) {
	g := &schemaGen{defs: map[string]any{}}
	root := g.object(configType)
	root["$schema"] = "http://json-schema.org/draft-07/schema#"
	root["$id"] = SchemaURL
	root["title"] = "dockpaum configuration"
	root["description"] = "Configuration of dockpaum, the policy-driven Docker container updater (dockpaum.yml). Values may reference environment variables as ${VAR}."
	g.defs["duration"] = map[string]any{
		"description": "A duration: 30s, 5m, 1h30m, 1d or 2w (Go syntax with d and w), or whole seconds.",
		"anyOf": []any{
			map[string]any{"type": "integer"},
			map[string]any{"type": "string", "pattern": durationPattern},
			ref("envRef"),
		},
	}
	g.defs["boolean"] = map[string]any{
		"description": "true or false; the YAML 1.1 spellings yes/no, on/off and y/n are accepted too.",
		"anyOf": []any{
			map[string]any{"type": "boolean"},
			map[string]any{"type": "string", "enum": yaml11Bools},
			ref("envRef"),
		},
	}
	g.defs["envRef"] = map[string]any{
		"description": "A reference to an environment variable (${VAR}, ${VAR:-default}, $VAR), expanded when the configuration loads.",
		"type":        "string",
		"pattern":     envRefPattern,
	}
	root["definitions"] = g.defs

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

type schemaGen struct {
	defs map[string]any
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/definitions/" + name}
}

func orEnv(s map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{s, ref("envRef")}}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	for _, f := range modelFields(t) {
		key := t.Name() + "." + f.key
		s := g.value(f.typ, key)
		if desc := schemaDescriptions[key]; desc != "" {
			if _, isRef := s["$ref"]; isRef {
				s = map[string]any{"allOf": []any{s}}
			}
			s["description"] = desc
		}
		props[f.key] = s
	}
	out := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if desc := schemaDescriptions[t.Name()]; desc != "" {
		out["description"] = desc
	}
	return out
}

func (g *schemaGen) value(t reflect.Type, key string) map[string]any {
	t = deref(t)
	switch {
	case t == durationType:
		return ref("duration")
	case t == scheduleType:
		return map[string]any{"anyOf": []any{
			g.definition(t),
			map[string]any{"type": "string", "description": "Shorthand: an interval (1h, 1d) or a cron spec (\"0 4 * * *\")."},
			map[string]any{"type": "integer", "description": "Shorthand: an interval in seconds."},
		}}
	case t.Kind() == reflect.Struct:
		return g.definition(t)
	case t.Kind() == reflect.Slice:
		elem := deref(t.Elem())
		items := g.value(elem, key)
		list := map[string]any{"type": "array", "items": items}
		switch {
		case elem == policyType:
			return map[string]any{"anyOf": []any{list, map[string]any{
				"type":                 "object",
				"description":          "Map form: policy settings keyed by policy id.",
				"additionalProperties": map[string]any{"anyOf": []any{items, map[string]any{"type": "null"}}},
			}}}
		case isScalarType(elem) || elem == durationType:
			return map[string]any{"anyOf": []any{list, items}}
		}
		return list
	case t.Kind() == reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.value(t.Elem(), key)}
	case t.Kind() == reflect.Bool:
		return ref("boolean")
	case t.Kind() == reflect.String:
		if enum := schemaEnums[key]; enum != nil {
			return orEnv(map[string]any{"type": "string", "enum": enum})
		}
		if rules := schemaRules[key]; rules != nil {
			return orEnv(withRules(map[string]any{"type": "string"}, rules))
		}
		return map[string]any{"type": "string"}
	}
	return orEnv(withRules(map[string]any{"type": "integer"}, schemaRules[key]))
}

func withRules(s, rules map[string]any) map[string]any {
	maps.Copy(s, rules)
	return s
}

func (g *schemaGen) definition(t reflect.Type) map[string]any {
	name := t.Name()
	if _, ok := g.defs[name]; !ok {
		g.defs[name] = nil
		g.defs[name] = g.object(t)
	}
	return ref(name)
}

var schemaEnums = map[string][]any{
	"LogConfig.level":             {"debug", "info", "warn", "warning", "error"},
	"LogConfig.format":            {"json", "text"},
	"UpdatePolicy.strategy":       {StrategyRolling, StrategyParallel},
	"Policy.mode":                 anySlice(modes),
	"Policy.min_image_age_source": anySlice(ageSources),
	"NotifierConfig.type":         anySlice(notifyTypes),
	"NotifierConfig.events":       anySlice(append(slices.Clone(notifyEvents), "*")),
	"ApprovalConfig.method":       anySlice(approvalMethods),
	"ApprovalConfig.approve_on":   anySlice(approveOnValues),
}

var schemaRules = map[string]map[string]any{
	"Config.instance":        {"maxLength": maxInstanceLen},
	"StateConfig.dir":        {"pattern": `^\s*(/|$)`},
	"SMTPConfig.port":        {"minimum": 1, "maximum": 65535},
	"ApprovalSigning.key_id": {"pattern": `^\s*[A-Za-z0-9._-]{0,64}\s*$`},
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

var schemaDescriptions = map[string]string{ // #nosec G101 -- descriptions of the keys, no credentials
	"Config.log":           "Logging.",
	"Config.http":          "The operational HTTP server: health, readiness, busy, version, metrics and the v1 API under /internal-api/.",
	"Config.timezone":      "IANA time zone (Europe/Berlin) of schedules, maintenance windows, policies and jobs that set none of their own. Default: schedule.timezone, else the TZ environment variable, else UTC. DOCKPAUM_TIMEZONE overrides it.",
	"Config.schedule":      "When update runs happen: a mapping, or the shorthand of an interval (1h) or a cron spec (\"0 4 * * *\"). DOCKPAUM_SCHEDULE overrides it.",
	"Config.hosts":         "Docker hosts. Default: one host named local, reached through the environment (DOCKER_HOST, else the local socket).",
	"Config.filters":       "Which containers dockpaum considers.",
	"Config.update":        "How containers are updated.",
	"Config.registry":      "Registry credentials; the most specific server entry wins.",
	"Config.notify":        "Notification channels. DOCKPAUM_NOTIFY_URLS adds channels from service URLs; DOCKPAUM_NOTIFY_EVENTS sets the events of channels that list none.",
	"Config.hooks":         "Commands run around update runs and container updates.",
	"Config.jobs":          "Scripts run on their own cron schedule, independent of updates and maintenance windows; skipped while dry_run is on.",
	"Config.audit":         "The JSONL audit log of every update decision.",
	"Config.dry_run":       "Check and report what would be updated, change nothing; jobs and pre_run/post_run hooks are skipped. Default false. DOCKPAUM_DRY_RUN overrides it; --dry-run and DOCKPAUM_WATCHTOWER_ENV=true turn it on.",
	"Config.docker_config": "Path of a Docker config.json whose inline auths are added after the registry entries, which win for the same server; credential stores and helpers are not supported. Default: config.json in $DOCKER_CONFIG when that is set.",
	"Config.state":         "The durable state file: soak first-seen times, image defaults, notification ledger and previous images.",
	"Config.instance":      "Name of this dockpaum instance in notifications and status: up to 64 characters, no line breaks, tabs or control characters. Default: the host name. DOCKPAUM_INSTANCE overrides it.",

	"LogConfig":        "Logging.",
	"LogConfig.level":  "debug, info, warn or error. Default info. DOCKPAUM_LOG_LEVEL overrides it.",
	"LogConfig.format": "json or text. Default json. DOCKPAUM_LOG_FORMAT overrides it.",

	"HTTPConfig":                   "The operational HTTP server.",
	"HTTPConfig.addr":              "Listen address such as 127.0.0.1:8080; wins over port.",
	"HTTPConfig.port":              "Listen port. Default 8080. DOCKPAUM_HTTP_PORT overrides it, also the port in addr.",
	"HTTPConfig.enabled":           "Serve the operational endpoints. Default true; false opens no port at all, and --healthcheck then reports healthy. DOCKPAUM_HTTP_ENABLED overrides it.",
	"HTTPConfig.token":             "Bearer token for every /internal-api/ route, health and readiness included, and for Watchtower's /v1/update and /v1/metrics; the update and check API accept only this token. Without token and read_token the read routes are open. Use ${VAR} or token_file; DOCKPAUM_HTTP_TOKEN (or DOCKPAUM_HTTP_TOKEN_FILE) overrides it.",
	"HTTPConfig.token_file":        "File holding the token (a Docker or Kubernetes secret), read once at startup.",
	"HTTPConfig.read_token":        "Read-only bearer token for the /internal-api/ read routes (health, ready, busy, version, metrics, status, containers, summary) and /v1/metrics; it never triggers updates or checks and must differ from token. DOCKPAUM_HTTP_READ_TOKEN (or DOCKPAUM_HTTP_READ_TOKEN_FILE) overrides it.",
	"HTTPConfig.read_token_file":   "File holding the read-only token, read once at startup.",
	"HTTPConfig.update_api":        "Enable POST /internal-api/v1/update (and GET or POST /v1/update with watchtower_compat) to queue a run on demand, and POST /internal-api/v1/check to queue a run that only checks and reports. Requires token. Default false. DOCKPAUM_HTTP_UPDATE_API overrides it.",
	"HTTPConfig.watchtower_compat": "Also serve Watchtower's HTTP API routes for its clients (dashboards such as Homepage, scripts): /v1/update (needs update_api and token) and /v1/metrics (authenticated like the /internal-api/ read routes). Default false.",

	"ScheduleConfig":              "When update runs happen.",
	"ScheduleConfig.cron":         "Cron spec: five fields (\"0 4 * * *\"), six with seconds first, or a descriptor such as @daily or @every 6h. A CRON_TZ=<zone> prefix overrides the time zone. Exclusive with interval.",
	"ScheduleConfig.interval":     "Run interval when no cron is set. Default " + formatDuration(DefaultInterval) + ".",
	"ScheduleConfig.timezone":     "Time zone of the cron and the global windows. Default: the top-level timezone.",
	"ScheduleConfig.run_on_start": "Also run once right at startup. Default: true in interval mode; in cron mode only while dry_run is on.",
	"ScheduleConfig.windows":      "Global maintenance windows: scheduled runs and policy schedules outside them do not start. Runs started through the API or with --once, and jobs, are not held back.",

	"WindowSpec":       "A maintenance window: days and time ranges.",
	"WindowSpec.days":  "Days: mon to sun, ranges such as Mon-Fri, or all, weekdays, weekends. Empty means every day.",
	"WindowSpec.times": "Time ranges HH:MM-HH:MM (22:00-02:00 crosses midnight), or always. Empty means all day.",

	"HostConfig":          "A Docker host.",
	"HostConfig.name":     "Name in logs, metrics and jobs[].host; unique. Default host-<index>.",
	"HostConfig.endpoint": "Empty for the environment (DOCKER_HOST, else the local socket), unix:///path, or tcp://host:2376 with tls. ssh:// is not supported.",
	"HostConfig.tls":      "TLS for a tcp:// endpoint.",

	"TLS":             "TLS for a tcp:// Docker host.",
	"TLS.ca_cert":     "CA certificate file.",
	"TLS.client_cert": "Client certificate file.",
	"TLS.client_key":  "Client key file.",
	"TLS.insecure":    "Skip certificate verification (not recommended); verify wins when both are set.",
	"TLS.verify":      "Verify the server certificate. Default true.",

	"FilterConfig":                   "Which containers dockpaum considers.",
	"FilterConfig.include_stopped":   "Also consider containers that are not running; they are recreated without being started. DOCKPAUM_INCLUDE_STOPPED overrides it.",
	"FilterConfig.include_names":     "Only these container names.",
	"FilterConfig.exclude_names":     "Never these container names.",
	"FilterConfig.include_regex":     "Only container names matching this RE2 regular expression.",
	"FilterConfig.exclude_regex":     "Never container names matching this RE2 regular expression.",
	"FilterConfig.label_enable":      "Opt-in mode: only containers labelled dockpaum.enable=true (or Watchtower's enable label). DOCKPAUM_LABEL_ENABLE overrides it.",
	"FilterConfig.scope":             "Only containers labelled dockpaum.scope=<scope>. Unset: containers with a scope label are left to the instance that owns it. DOCKPAUM_SCOPE overrides it.",
	"FilterConfig.watchtower_labels": "Honor com.centurylinklabs.watchtower.* labels; a dockpaum.* label wins. Default true.",

	"UpdatePolicy":                     "How containers are updated.",
	"UpdatePolicy.strategy":            "rolling (one at a time) or parallel (up to concurrency; not with compose_group). Default rolling.",
	"UpdatePolicy.concurrency":         "Parallel updates with strategy parallel. Default 1.",
	"UpdatePolicy.stop_timeout":        "Graceful stop timeout of the old container; a longer stop timeout set on the container wins. Default 30s.",
	"UpdatePolicy.cleanup_images":      "Remove the old image after an update when no tag or container still uses it. DOCKPAUM_CLEANUP overrides it.",
	"UpdatePolicy.restart_unchanged":   "Recreate containers even when their image did not change.",
	"UpdatePolicy.rollback_on_failure": "Restore the old container when the new one fails to start or turn healthy. Default true. DOCKPAUM_ROLLBACK overrides it.",
	"UpdatePolicy.approval":            "An HTTP approval request before every update.",
	"UpdatePolicy.policies":            "Update policies, as a list or as a mapping keyed by policy id. A container follows the policy its dockpaum.policy label names, else the default policy: a configured one, else the built-in tag-follow, patch, minor, major or monitor, which take min_image_age, health_timeout and windows from the default policy; none and off leave it alone.",
	"UpdatePolicy.compose_group":       "Update the containers of each Compose project one after another, ordered by depends_on; strategy parallel then has no effect.",
	"UpdatePolicy.self_update":         "How dockpaum updates its own container.",

	"ApprovalConfig":                     "An HTTP approval request before every update: a 2xx answer (or ok_statuses) approves unless its JSON body says \"approved\": false; a denial may name a retry time (Retry-After, retry_after_seconds). With approve_on: failure the answer is read the other way round.",
	"ApprovalConfig.url":                 "Approval endpoint (http or https).",
	"ApprovalConfig.method":              "HTTP method. Default POST; GET, HEAD and DELETE send the request as query parameters.",
	"ApprovalConfig.headers":             "Request headers, such as Authorization: Bearer ${TOKEN}.",
	"ApprovalConfig.header_files":        "Headers whose values are read from files, read once at startup.",
	"ApprovalConfig.timeout":             "Request timeout. Default 10s.",
	"ApprovalConfig.fail_open":           "Approve when the endpoint cannot be reached (not recommended). No effect with approve_on: failure.",
	"ApprovalConfig.ok_statuses":         "Status codes that approve instead of any 2xx.",
	"ApprovalConfig.label_allowlist":     "Container labels sent besides dockpaum.* and com.docker.compose.*; \"*\" sends all.",
	"ApprovalConfig.signing":             "Sign every approval request (X-Dockpaum-Signature, DOCKPAUM-APPROVAL-V1) with an HMAC-SHA256 key.",
	"ApprovalConfig.recheck_before_stop": "Ask again (phase final) after the pull and the pre-update hooks, right before the old container is stopped; a denial skips the update without stopping anything.",
	"ApprovalConfig.approve_on":          "success (default): a 2xx answer (or ok_statuses) approves. failure: the url is a status URL, and its 2xx answer (or ok_statuses) denies while any other status, or an endpoint that cannot be reached, approves; the body's approved field, Retry-After and fail_open are ignored, a signing failure still denies.",

	"ApprovalSigning":             "The key approval requests are signed with.",
	"ApprovalSigning.key_id":      "Key ID sent in X-Dockpaum-Key-Id, so the approver can accept several keys during a rotation. Default default.",
	"ApprovalSigning.secret_file": "File holding the HMAC secret (at least 32 bytes, surrounding whitespace trimmed), read again whenever it changes.",

	"Policy":                      "An update policy. A container selects it with the label dockpaum.policy=<id>.",
	"Policy.id":                   "Policy id, matched case-insensitively. Empty or default is the fallback for unlabelled containers; none and off are reserved. An id equal to a built-in (tag-follow, patch, minor, major, monitor) replaces it.",
	"Policy.mode":                 "tag-follow follows the digest of the current tag; patch, minor and major move to newer version tags within that range; none never checks or updates. digest is another name for tag-follow. Default tag-follow. DOCKPAUM_MODE sets it for the default policy.",
	"Policy.min_image_age":        "Skip a new image younger than this (soak time). Default 1h (the config dockpaum init writes sets 0s); 0s turns it off. DOCKPAUM_MIN_IMAGE_AGE sets it for the default policy.",
	"Policy.health_timeout":       "How long the new container has to turn healthy, else the update fails and is rolled back (rollback_on_failure); one without a healthcheck must keep running through a grace period of up to 15s. Default 2m; 0s turns the health gate off. DOCKPAUM_HEALTH_TIMEOUT sets it for the default policy.",
	"Policy.timezone":             "Time zone of this policy's windows and schedule. Default: the top-level timezone.",
	"Policy.windows":              "Maintenance windows: containers of this policy are updated only inside them.",
	"Policy.schedule":             "The policy's own cron spec: its containers are left out of regular runs and updated when it fires.",
	"Policy.hooks":                "Commands for containers of this policy, after the global hooks of the same phase.",
	"Policy.monitor_only":         "Check for updates and report them (audit decision monitor-only, event update.available), never recreate. Unlike mode none, which never checks.",
	"Policy.tags":                 "Narrow (include, exclude) and reshape (transform) the version tags patch, minor and major consider.",
	"Policy.min_image_age_source": "How min_image_age is measured: auto (the image's creation time, or when dockpaum first saw the image if that time is unusable, as with reproducible builds), first_seen (from when dockpaum first saw the image, unless it was built later; needs a persistent state) or created (the creation time only). Default auto.",

	"PolicyHooks":             "Commands for containers of a policy.",
	"PolicyHooks.pre_update":  "Before each update of a container of this policy.",
	"PolicyHooks.post_update": "After each successful update.",
	"PolicyHooks.on_failure":  "When an update fails.",

	"TagFilter":           "Tag rules: RE2 regular expressions over version tags, matched anywhere in the tag (anchor with ^ and $), and a transform.",
	"TagFilter.include":   "A candidate tag must match this, when set.",
	"TagFilter.exclude":   "A candidate tag must not match this.",
	"TagFilter.transform": "Rewrite a tag into the version that is compared: \"<regex> => <template>\", such as \"^release-(.+)$ => $1\". The template names groups as $1, ${1} or $${name} (the $$ keeps ${name} from being read as an environment variable). A tag the regex does not match is compared unchanged.",

	"SelfUpdateConfig":              "How dockpaum updates its own container, through a helper container.",
	"SelfUpdateConfig.enabled":      "Let dockpaum update its own container. Default false. DOCKPAUM_SELF_UPDATE overrides it.",
	"SelfUpdateConfig.delay":        "How long the helper waits before replacing dockpaum. Default 5s.",
	"SelfUpdateConfig.helper_image": "Image of the helper container. Default: the new dockpaum image.",

	"RegistryAuth":               "Credentials for a registry, or for one repository path on it.",
	"RegistryAuth.server":        "Registry host, optionally with a repository path (ghcr.io/acme) to scope the credentials.",
	"RegistryAuth.username":      "User name. Required with password_file.",
	"RegistryAuth.password":      "Password or token. Use ${VAR} or password_file.",
	"RegistryAuth.password_file": "File holding the password or token, read on every use so a rotated token needs no restart.",

	"NotifierConfig":              "A notification channel. With a service URL (discord://, ntfy://, smtp://, ...) or a Discord, Slack or Teams workflow webhook URL, type may be left out.",
	"NotifierConfig.type":         "Channel type; may be left out with a service URL or a well-known webhook URL.",
	"NotifierConfig.name":         "Name in logs and metrics. Default: the type.",
	"NotifierConfig.events":       "Events to deliver; * means all. Empty means update.success, update.failure and run.completed, the latter only for runs that changed, failed or found something new. The events: update.success, update.failure, update.available (dry-run, monitor-only), update.skipped (held back by a gate), update.denied (approval denied), update.rolled_back, update.recovered (an interrupted update recovered, a network reattached), check.failed (registry check failed or incomplete), self_update.started, self_update.success, self_update.failure, run.started, run.completed, dockpaum.started, dockpaum.stopping and notify.test.",
	"NotifierConfig.url":          "Webhook URL, the ntfy topic URL, the Gotify or Matrix server, the Apprise API endpoint, or a service URL. Use ${VAR} or url_file.",
	"NotifierConfig.url_file":     "File holding the URL, read once at startup.",
	"NotifierConfig.token":        "Bot token (telegram), application token (gotify, pushover), access token (matrix) or bearer token (ntfy, webhook).",
	"NotifierConfig.token_file":   "File holding the token, read once at startup.",
	"NotifierConfig.channel":      "Slack channel override, the Telegram chat id, the Pushover device(s), or the Apprise tags.",
	"NotifierConfig.from":         "Email sender.",
	"NotifierConfig.to":           "Email recipients, or the Apprise service URLs of an apprise channel.",
	"NotifierConfig.smtp":         "SMTP server of an email channel.",
	"NotifierConfig.headers":      "Extra HTTP headers (webhook, apprise).",
	"NotifierConfig.header_files": "Headers whose values are read from files, read once at startup.",
	"NotifierConfig.user":         "Pushover user or group key.",
	"NotifierConfig.room":         "Matrix room ID (!id:server) or alias (#alias:server).",
	"NotifierConfig.once":         "Report each standing notice (an available update in dry-run or monitor-only, a skipped or denied update, an incomplete check) once until it changes; false repeats it every run. Default true.",

	"SMTPConfig":               "SMTP server of an email channel.",
	"SMTPConfig.host":          "SMTP host.",
	"SMTPConfig.port":          "SMTP port, 1 to 65535. Default 587; 465 uses implicit TLS.",
	"SMTPConfig.username":      "SMTP user name.",
	"SMTPConfig.password":      "SMTP password. Use ${VAR} or password_file.",
	"SMTPConfig.password_file": "File holding the SMTP password, read once at startup.",
	"SMTPConfig.starttls":      "Require STARTTLS; without it, STARTTLS is used only when the server offers it.",

	"HookConfig":             "Commands run around update runs and container updates: host commands, or exec:<container>:<command> inside a container (@self is the updated one).",
	"HookConfig.pre_run":     "Host commands before every update run; a failure aborts the run.",
	"HookConfig.post_run":    "Host commands after every run whose pre_run succeeded.",
	"HookConfig.pre_update":  "Before each container update; exit code 75 skips the update.",
	"HookConfig.post_update": "After each successful update.",
	"HookConfig.on_failure":  "When an update fails.",
	"HookConfig.timeout":     "Timeout per command. Default 60s.",
	"HookConfig.disabled":    "Turn every hook off, label hooks included (not jobs).",
	"HookConfig.label_hooks": "Run commands from dockpaum.hook.* (and Watchtower lifecycle) container labels inside that container. Default false.",

	"JobConfig":          "A script run on its own cron schedule.",
	"JobConfig.name":     "Unique job name.",
	"JobConfig.cron":     "Cron spec, the same syntax as schedule.cron.",
	"JobConfig.timezone": "Time zone of the cron. Default: the top-level timezone.",
	"JobConfig.commands": "Commands run in order; the first failure stops the job.",
	"JobConfig.host":     "Docker host for exec: commands; required with several hosts.",
	"JobConfig.timeout":  "Timeout per command. Default: hooks.timeout.",

	"AuditConfig":           "The JSONL audit log of every update decision.",
	"AuditConfig.enabled":   "Write audit records. Default true.",
	"AuditConfig.dir":       "Audit directory. Default /var/lib/dockpaum/audit.",
	"AuditConfig.retention": "How long records are kept. Default 31d.",

	"StateConfig":         "Where dockpaum keeps its durable state.",
	"StateConfig.enabled": "Keep state on disk. Default true; false keeps it in memory only.",
	"StateConfig.dir":     "Directory of state.json, an absolute path; mount it as a volume to keep the state across restarts and self-updates. Default /var/lib/dockpaum/state. DOCKPAUM_STATE_DIR overrides it.",
}
