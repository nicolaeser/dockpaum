package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/nicolaeser/dockpaum/internal/window"
)

//go:embed default.yml
var defaultConfigYAML []byte

func DefaultConfig() []byte {
	out := make([]byte, len(defaultConfigYAML))
	copy(out, defaultConfigYAML)
	return out
}

const EnvConfigURL = "DOCKPAUM_CONFIG_URL"

const (
	DefaultInterval      = 5 * time.Minute
	DefaultMinImageAge   = time.Hour
	DefaultHealthTimeout = 2 * time.Minute
	DefaultAuditDir      = "/var/lib/dockpaum/audit"
	DefaultStateDir      = "/var/lib/dockpaum/state"
	DefaultSMTPPort      = 587
)

const stateFileName = "state.json"

const (
	AgeSourceAuto      = "auto"
	AgeSourceFirstSeen = "first_seen"
	AgeSourceCreated   = "created"
)

var ageSources = []string{AgeSourceAuto, AgeSourceFirstSeen, AgeSourceCreated}

func ValidAgeSource(s string) bool {
	return s == "" || slices.Contains(ageSources, s)
}

type Config struct {
	Log          LogConfig        `yaml:"log"`
	HTTP         HTTPConfig       `yaml:"http"`
	Timezone     string           `yaml:"timezone"`
	Schedule     ScheduleConfig   `yaml:"schedule"`
	Hosts        []HostConfig     `yaml:"hosts"`
	Filters      FilterConfig     `yaml:"filters"`
	Update       UpdatePolicy     `yaml:"update"`
	Registry     []RegistryAuth   `yaml:"registry"`
	Notify       []NotifierConfig `yaml:"notify"`
	Hooks        HookConfig       `yaml:"hooks"`
	Jobs         []JobConfig      `yaml:"jobs"`
	Audit        AuditConfig      `yaml:"audit"`
	DryRun       bool             `yaml:"dry_run"`
	DockerConfig string           `yaml:"docker_config"`
	State        StateConfig      `yaml:"state"`
	Instance     string           `yaml:"instance"`

	source       string
	loadWarnings []string
	envOverrides []EnvOverride
	runOnce      bool
	notifyOrigin []string
	lines        lineIndex
}

func (c *Config) Source() string { return c.source }

func (c *Config) RunOnStart() bool {
	return c.Schedule.ShouldRunOnStart(c.DryRun)
}

type AuditConfig struct {
	Enabled   *bool    `yaml:"enabled"`
	Dir       string   `yaml:"dir"`
	Retention Duration `yaml:"retention"`
}

func (a AuditConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return a.Dir != ""
	}
	return *a.Enabled
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type HTTPConfig struct {
	Addr             string `yaml:"addr"`
	Port             int    `yaml:"port"`
	Enabled          *bool  `yaml:"enabled"`
	Token            string `yaml:"token"`
	TokenFile        string `yaml:"token_file"`
	ReadToken        string `yaml:"read_token"`
	ReadTokenFile    string `yaml:"read_token_file"`
	UpdateAPI        bool   `yaml:"update_api"`
	WatchtowerCompat bool   `yaml:"watchtower_compat"`
}

type StateConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Dir     string `yaml:"dir"`
}

func (s StateConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

func (s StateConfig) Path() string {
	if !s.IsEnabled() {
		return ""
	}
	dir := strings.TrimSpace(s.Dir)
	if dir == "" {
		dir = DefaultStateDir
	}
	return filepath.Join(dir, stateFileName)
}

func (h HTTPConfig) listenPort() int {
	if _, p, err := net.SplitHostPort(h.ListenAddr()); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 8080
}

func (h HTTPConfig) IsEnabled() bool {
	if h.Enabled == nil {
		return true
	}
	return *h.Enabled
}

func (h HTTPConfig) ListenAddr() string {
	if h.Addr != "" {
		return h.Addr
	}
	if h.Port > 0 {
		return fmt.Sprintf(":%d", h.Port)
	}
	return ":8080"
}

type ScheduleConfig struct {
	Cron       string       `yaml:"cron"`
	Interval   Duration     `yaml:"interval"`
	Timezone   string       `yaml:"timezone"`
	RunOnStart *bool        `yaml:"run_on_start"`
	Windows    []WindowSpec `yaml:"windows"`
}

func (s ScheduleConfig) ShouldRunOnStart(dryRun bool) bool {
	if s.RunOnStart != nil {
		return *s.RunOnStart
	}
	return s.Cron == "" || dryRun
}

func (s ScheduleConfig) CronSpec() string {
	return withTimezone(s.Cron, s.Timezone)
}

type HostConfig struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"`
	TLS      *TLS   `yaml:"tls"`
}

type TLS struct {
	CACert     string `yaml:"ca_cert"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	Insecure   bool   `yaml:"insecure"`
	Verify     *bool  `yaml:"verify"`
}

func (t *TLS) ShouldVerify() bool {
	if t == nil {
		return true
	}
	if t.Verify != nil {
		return *t.Verify
	}
	return !t.Insecure
}

type FilterConfig struct {
	IncludeStopped   bool     `yaml:"include_stopped"`
	IncludeNames     []string `yaml:"include_names"`
	ExcludeNames     []string `yaml:"exclude_names"`
	IncludeRegex     string   `yaml:"include_regex"`
	ExcludeRegex     string   `yaml:"exclude_regex"`
	LabelEnable      bool     `yaml:"label_enable"`
	Scope            string   `yaml:"scope"`
	WatchtowerLabels *bool    `yaml:"watchtower_labels"`

	includeRe *regexp.Regexp
	excludeRe *regexp.Regexp
}

func (f FilterConfig) HonorWatchtowerLabels() bool {
	return f.WatchtowerLabels == nil || *f.WatchtowerLabels
}

func (f *FilterConfig) IncludeRegexp() *regexp.Regexp { return f.includeRe }
func (f *FilterConfig) ExcludeRegexp() *regexp.Regexp { return f.excludeRe }

type UpdatePolicy struct {
	Strategy       string          `yaml:"strategy"`
	Concurrency    int             `yaml:"concurrency"`
	StopTimeout    Duration        `yaml:"stop_timeout"`
	CleanupImages  bool            `yaml:"cleanup_images"`
	IncludeRestart bool            `yaml:"restart_unchanged"`
	RollbackOnFail *bool           `yaml:"rollback_on_failure"`
	Approval       *ApprovalConfig `yaml:"approval"`

	Policies []Policy `yaml:"policies"`

	ComposeGroup bool `yaml:"compose_group"`

	SelfUpdate SelfUpdateConfig `yaml:"self_update"`
}

func (u UpdatePolicy) Rollback() bool {
	return u.RollbackOnFail == nil || *u.RollbackOnFail
}

type SelfUpdateConfig struct {
	Enabled     *bool    `yaml:"enabled"`
	Delay       Duration `yaml:"delay"`
	HelperImage string   `yaml:"helper_image"`
}

func (s SelfUpdateConfig) IsEnabled() bool {
	return s.Enabled != nil && *s.Enabled
}

type Policy struct {
	ID                string       `yaml:"id"`
	Mode              string       `yaml:"mode"`
	MinImageAge       Duration     `yaml:"min_image_age"`
	HealthTimeout     Duration     `yaml:"health_timeout"`
	Timezone          string       `yaml:"timezone"`
	Windows           []WindowSpec `yaml:"windows"`
	Schedule          string       `yaml:"schedule"`
	Hooks             PolicyHooks  `yaml:"hooks"`
	MonitorOnly       bool         `yaml:"monitor_only"`
	Tags              TagFilter    `yaml:"tags,omitempty"`
	MinImageAgeSource string       `yaml:"min_image_age_source"`

	minImageAgeSet   bool
	healthTimeoutSet bool
}

func DefaultPolicy() Policy {
	return Policy{
		ID:                "default",
		Mode:              ModeTagFollow,
		MinImageAge:       Duration(DefaultMinImageAge),
		HealthTimeout:     Duration(DefaultHealthTimeout),
		MinImageAgeSource: AgeSourceAuto,
		minImageAgeSet:    true,
		healthTimeoutSet:  true,
	}
}

type PolicyHooks struct {
	PreUpdate  []string `yaml:"pre_update"`
	PostUpdate []string `yaml:"post_update"`
	OnFailure  []string `yaml:"on_failure"`
}

func (p Policy) CronSpec() string {
	return withTimezone(p.Schedule, p.Timezone)
}

type WindowSpec struct {
	Days  []string `yaml:"days"`
	Times []string `yaml:"times"`
}

func (p Policy) IsDefault() bool {
	id := strings.ToLower(strings.TrimSpace(p.ID))
	return id == "" || id == "default"
}

type ApprovalConfig struct {
	URL               string            `yaml:"url"`
	Method            string            `yaml:"method"`
	Headers           map[string]string `yaml:"headers"`
	HeaderFiles       map[string]string `yaml:"header_files"`
	Timeout           Duration          `yaml:"timeout"`
	FailOpen          bool              `yaml:"fail_open"`
	OkStatuses        []int             `yaml:"ok_statuses"`
	LabelAllowlist    []string          `yaml:"label_allowlist"`
	Signing           *ApprovalSigning  `yaml:"signing"`
	RecheckBeforeStop bool              `yaml:"recheck_before_stop"`
	ApproveOn         string            `yaml:"approve_on"`
}

const (
	ApproveOnSuccess = "success"
	ApproveOnFailure = "failure"
)

var approveOnValues = []string{ApproveOnSuccess, ApproveOnFailure}

func (a ApprovalConfig) ApprovesOnFailure() bool { return a.ApproveOn == ApproveOnFailure }

type ApprovalSigning struct {
	KeyID      string `yaml:"key_id"`
	SecretFile string `yaml:"secret_file"`
}

const (
	DefaultApprovalKeyID = "default"
	MinApprovalSecretLen = 32
)

var approvalKeyIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var approvalSigningHeaders = []string{"X-Dockpaum-Key-Id", "X-Dockpaum-Timestamp", "X-Dockpaum-Nonce", "X-Dockpaum-Signature"}

func (s ApprovalSigning) Secret() ([]byte, error) {
	v, err := readSecretFile(s.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("update.approval.signing.secret_file: %w", err)
	}
	if len(v) < MinApprovalSecretLen {
		return nil, fmt.Errorf("update.approval.signing.secret_file: %s holds %d bytes, at least %d are required", s.SecretFile, len(v), MinApprovalSecretLen)
	}
	return []byte(v), nil
}

type RegistryAuth struct {
	Server       string `yaml:"server"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

func (a RegistryAuth) Scope() (host, path string) {
	s := strings.TrimSpace(a.Server)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	host, path, _ = strings.Cut(s, "/")
	return strings.ToLower(host), strings.ToLower(strings.Trim(path, "/"))
}

const maxPasswordFileSize = maxSecretFileSize

func (a RegistryAuth) Credentials() (username, password string, err error) {
	if a.PasswordFile == "" {
		return a.Username, a.Password, nil
	}
	pw, err := readSecretFile(a.PasswordFile)
	if err != nil {
		return "", "", fmt.Errorf("registry %s: password_file: %w", a.Server, err)
	}
	return a.Username, pw, nil
}

type NotifierConfig struct {
	Type        string            `yaml:"type"`
	Name        string            `yaml:"name"`
	Events      []string          `yaml:"events"`
	URL         string            `yaml:"url"`
	URLFile     string            `yaml:"url_file"`
	Token       string            `yaml:"token"`
	TokenFile   string            `yaml:"token_file"`
	Channel     string            `yaml:"channel"`
	From        string            `yaml:"from"`
	To          []string          `yaml:"to"`
	SMTP        *SMTPConfig       `yaml:"smtp"`
	Headers     map[string]string `yaml:"headers"`
	HeaderFiles map[string]string `yaml:"header_files"`
	User        string            `yaml:"user"`
	Room        string            `yaml:"room"`
	Once        *bool             `yaml:"once"`
}

func (n NotifierConfig) ReportsOnce() bool {
	return n.Once == nil || *n.Once
}

type SMTPConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	StartTLS     bool   `yaml:"starttls"`
}

type HookConfig struct {
	PreRun     []string `yaml:"pre_run"`
	PostRun    []string `yaml:"post_run"`
	PreUpdate  []string `yaml:"pre_update"`
	PostUpdate []string `yaml:"post_update"`
	OnFailure  []string `yaml:"on_failure"`
	Timeout    Duration `yaml:"timeout"`
	Disabled   bool     `yaml:"disabled"`
	LabelHooks bool     `yaml:"label_hooks"`
}

type JobConfig struct {
	Name     string   `yaml:"name"`
	Cron     string   `yaml:"cron"`
	Timezone string   `yaml:"timezone"`
	Commands []string `yaml:"commands"`
	Host     string   `yaml:"host"`
	Timeout  Duration `yaml:"timeout"`
}

func (j JobConfig) CronSpec() string {
	return withTimezone(j.Cron, j.Timezone)
}

const (
	StrategyRolling  = "rolling"
	StrategyParallel = "parallel"

	ModeTagFollow = "tag-follow"
	ModeDigest    = "digest"
	ModeNone      = "none"
	ModePatch     = "patch"
	ModeMinor     = "minor"
	ModeMajor     = "major"
)

func ValidMode(m string) bool {
	switch m {
	case ModeTagFollow, ModeDigest, ModeNone, ModePatch, ModeMinor, ModeMajor, "":
		return true
	}
	return false
}

const embeddedSource = "embedded default"

func Load(path string) (*Config, error) {
	raw, source, warning, err := fetchOrEnsure(path)
	if err != nil {
		return nil, err
	}
	c, err := decode(raw, source)
	if err != nil {
		return nil, err
	}
	if warning != "" {
		c.loadWarnings = append(c.loadWarnings, warning)
	}
	return c, nil
}

func Validate(path string) (*Config, error) {
	if url := strings.TrimSpace(os.Getenv(EnvConfigURL)); url != "" {
		body, err := fetchURL(url)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}
		return decode(body, url)
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && isEmptyConfig(body)) {
		return decode(defaultConfigYAML, embeddedSource)
	}
	if err != nil {
		return nil, readError(path, err)
	}
	return decode(body, path)
}

func readError(path string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, syscall.EISDIR) {
		return fmt.Errorf("read %s: %w — mount the config file itself, or point --config (DOCKPAUM_CONFIG) at a file", path, err)
	}
	return fmt.Errorf("read %s: %w", path, err)
}

func isEmptyConfig(b []byte) bool {
	return len(bytes.TrimSpace(b)) == 0
}

func decode(raw []byte, source string) (*Config, error) {
	return decodeEnv(raw, source, systemEnv())
}

func decodeEnv(raw []byte, source string, env envMap) (*Config, error) {
	expanded, err := expandEnv(string(raw), os.LookupEnv)
	if err != nil {
		var rerr *requiredVarError
		if errors.As(err, &rerr) {
			return nil, &Error{Source: source, Problems: []Problem{{Line: rerr.line, Message: rerr.Error(), err: err}}}
		}
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	doc, err := parseDocument(source, raw, expanded)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := doc.decode(&c); err != nil {
		return nil, err
	}
	c.source = source
	c.lines = doc.lines
	c.loadWarnings = append(c.loadWarnings, doc.warnings...)
	c.normalize()
	if err := c.validateNoDeferredRefs(); err != nil {
		return nil, doc.failErr(err)
	}
	if err := c.resolveSecretFiles(); err != nil {
		return nil, doc.failErr(err)
	}
	if err := c.expandNotifyURLs(); err != nil {
		return nil, doc.failErr(err)
	}
	if err := c.applyEnv(env); err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	c.applyDefaults()
	if err := c.resolveTimezones(env["TZ"]); err != nil {
		return nil, doc.failErr(err)
	}
	if err := c.validate(); err != nil {
		return nil, doc.failErr(err)
	}
	return &c, nil
}

func (c *Config) notifyLabel(i int) string {
	if i < len(c.notifyOrigin) && c.notifyOrigin[i] != "" {
		return c.notifyOrigin[i]
	}
	return fmt.Sprintf("notify[%d]", i)
}

func (c *Config) expandNotifyURLs() error {
	out := make([]NotifierConfig, 0, len(c.Notify))
	origin := make([]string, 0, len(c.Notify))
	for i, n := range c.Notify {
		label := fmt.Sprintf("notify[%d]", i)
		if n.Type == "" {
			n.Type = webhookType(n.URL)
		}
		if !isServiceURL(n.URL) {
			out = append(out, n)
			origin = append(origin, label)
			continue
		}
		key := label + ".url"
		if n.URLFile != "" {
			key = label + ".url_file"
		}
		if extra := serviceURLExtraFields(n); len(extra) > 0 {
			return fmt.Errorf("%s: with a service URL, set only type, name and events next to it (the URL carries the rest); found %s", label, strings.Join(extra, ", "))
		}
		conv, notes, err := convertServiceURL(n.URL)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if n.Type != "" && n.Type != conv[0].Type {
			return fmt.Errorf("%s: type %s does not match the %s:// URL, which is a %s notifier — leave type out", label, n.Type, urlScheme(n.URL), conv[0].Type)
		}
		for _, note := range notes {
			c.loadWarnings = append(c.loadWarnings, label+": "+note)
		}
		base := n.Name
		if base == "" {
			base = conv[0].Type
		}
		for j, cn := range conv {
			cn.Events = n.Events
			cn.URLFile = n.URLFile
			switch {
			case j > 0:
				cn.Name = fmt.Sprintf("%s-%d", base, j+1)
			case len(conv) > 1 || n.Name != "":
				cn.Name = base
			}
			out = append(out, cn)
			origin = append(origin, label)
		}
	}
	c.Notify, c.notifyOrigin = out, origin
	return nil
}

func serviceURLExtraFields(n NotifierConfig) []string {
	var extra []string
	for _, f := range []struct {
		key string
		set bool
	}{
		{"token", n.Token != "" || n.TokenFile != ""},
		{"channel", n.Channel != ""},
		{"from", n.From != ""},
		{"to", len(n.To) > 0},
		{"smtp", n.SMTP != nil},
		{"headers", len(n.Headers) > 0 || len(n.HeaderFiles) > 0},
		{"user", n.User != ""},
		{"room", n.Room != ""},
	} {
		if f.set {
			extra = append(extra, f.key)
		}
	}
	return extra
}

type keyPresent bool

func (k *keyPresent) UnmarshalYAML(*yaml.Node) error {
	*k = true
	return nil
}

func (c *Config) markExplicitGates(doc string) error {
	var shadow struct {
		Update struct {
			Policies []struct {
				MinImageAge   keyPresent `yaml:"min_image_age"`
				HealthTimeout keyPresent `yaml:"health_timeout"`
			} `yaml:"policies"`
		} `yaml:"update"`
	}
	if err := yaml.Unmarshal([]byte(doc), &shadow); err != nil {
		return err
	}
	for i := range c.Update.Policies {
		if i >= len(shadow.Update.Policies) {
			break
		}
		c.Update.Policies[i].minImageAgeSet = bool(shadow.Update.Policies[i].MinImageAge)
		c.Update.Policies[i].healthTimeoutSet = bool(shadow.Update.Policies[i].HealthTimeout)
	}
	return nil
}

func fetchOrEnsure(path string) (raw []byte, source, warning string, err error) {
	if url := strings.TrimSpace(os.Getenv(EnvConfigURL)); url != "" {
		body, err := fetchURL(url)
		if err != nil {
			return nil, url, "", fmt.Errorf("fetch %s: %w", url, err)
		}
		return body, url, "", nil
	}

	body, err := os.ReadFile(path)
	if err == nil && !isEmptyConfig(body) {
		return body, path, "", nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, path, "", readError(path, err)
	}
	if werr := writeDefault(path); werr != nil {
		return DefaultConfig(), embeddedSource, fmt.Sprintf("%v: running on the embedded default from memory; mount a writable directory or provide a config file", werr), nil
	}
	return DefaultConfig(), path, "", nil
}

func writeDefault(path string) error {
	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
	}
	if err := os.WriteFile(path, defaultConfigYAML, 0o600); err != nil {
		return fmt.Errorf("write default config to %s: %w", path, err)
	}
	return nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}

const EnvInsecureConfigURL = "DOCKPAUM_INSECURE_CONFIG_URL"

func fetchURL(rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("config url: %w", err)
	}
	insecure := strings.EqualFold(os.Getenv(EnvInsecureConfigURL), "true")
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) && !insecure {
		return nil, fmt.Errorf("config url %q must use https — a plain-http config can inject hooks that execute commands (set %s=true to override)", rawURL, EnvInsecureConfigURL)
	}
	cli := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("config url: too many redirects")
			}
			if req.URL.Scheme != "https" && !isLoopbackHost(req.URL.Hostname()) && !insecure {
				return fmt.Errorf("config url: refusing redirect to %s (https required)", req.URL.Redacted())
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil) // #nosec G704 -- admin-provided config URL by design; https enforced above
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/yaml, text/yaml, text/plain, */*")
	req.Header.Set("User-Agent", "dockpaum/config-fetch")
	resp, err := cli.Do(req) // #nosec G704 -- see above
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (c *Config) normalize() {
	lower := func(s *string) { *s = strings.ToLower(strings.TrimSpace(*s)) }
	lower(&c.Log.Level)
	lower(&c.Log.Format)
	lower(&c.Update.Strategy)
	c.Filters.Scope = strings.TrimSpace(c.Filters.Scope)
	c.Instance = strings.TrimSpace(c.Instance)
	c.State.Dir = strings.TrimSpace(c.State.Dir)
	for i := range c.Update.Policies {
		lower(&c.Update.Policies[i].Mode)
		lower(&c.Update.Policies[i].MinImageAgeSource)
	}
	for i := range c.Notify {
		n := &c.Notify[i]
		lower(&n.Type)
		for j := range n.Events {
			lower(&n.Events[j])
		}
	}
	if a := c.Update.Approval; a != nil {
		a.Method = strings.ToUpper(strings.TrimSpace(a.Method))
		lower(&a.ApproveOn)
		if s := a.Signing; s != nil {
			s.KeyID = strings.TrimSpace(s.KeyID)
			s.SecretFile = strings.TrimSpace(s.SecretFile)
			if s.KeyID == "" {
				s.KeyID = DefaultApprovalKeyID
			}
		}
	}
}

func (c *Config) applyDefaults() {
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	if c.HTTP.Addr == "" && c.HTTP.Port == 0 {
		c.HTTP.Port = 8080
	}
	if c.Schedule.Cron == "" && c.Schedule.Interval == 0 {
		c.Schedule.Interval = Duration(DefaultInterval)
	}
	if len(c.Hosts) == 0 {
		c.Hosts = []HostConfig{{Name: "local", Endpoint: ""}}
	}
	for i := range c.Hosts {
		if c.Hosts[i].Name == "" {
			c.Hosts[i].Name = fmt.Sprintf("host-%d", i)
		}
	}
	if c.Update.Strategy == "" {
		c.Update.Strategy = StrategyRolling
	}
	if c.Update.Concurrency <= 0 {
		c.Update.Concurrency = 1
	}
	if c.Update.StopTimeout == 0 {
		c.Update.StopTimeout = Duration(30 * time.Second)
	}
	if c.Hooks.Timeout == 0 {
		c.Hooks.Timeout = Duration(60 * time.Second)
	}
	for i := range c.Jobs {
		if c.Jobs[i].Timeout == 0 {
			c.Jobs[i].Timeout = c.Hooks.Timeout
		}
	}
	for _, n := range c.Notify {
		if n.SMTP != nil && n.SMTP.Port == 0 {
			n.SMTP.Port = DefaultSMTPPort
		}
	}
	hasDefault := false
	for i := range c.Update.Policies {
		p := &c.Update.Policies[i]
		if p.Mode == "" {
			p.Mode = ModeTagFollow
		}
		if !p.minImageAgeSet && p.MinImageAge == 0 {
			p.MinImageAge = Duration(DefaultMinImageAge)
		}
		if !p.healthTimeoutSet && p.HealthTimeout == 0 {
			p.HealthTimeout = Duration(DefaultHealthTimeout)
		}
		if p.MinImageAgeSource == "" {
			p.MinImageAgeSource = AgeSourceAuto
		}
		hasDefault = hasDefault || p.IsDefault()
	}
	if !hasDefault {
		c.Update.Policies = append(c.Update.Policies, DefaultPolicy())
	}
	if c.Audit.Retention == 0 {
		c.Audit.Retention = Duration(31 * 24 * time.Hour)
	}
	if c.Audit.Dir == "" && (c.Audit.Enabled == nil || *c.Audit.Enabled) {
		c.Audit.Dir = DefaultAuditDir
	}
	if c.State.Dir == "" && c.State.IsEnabled() {
		c.State.Dir = DefaultStateDir
	}
}

func (c *Config) validate() error {
	if !slices.Contains([]string{StrategyRolling, StrategyParallel}, c.Update.Strategy) {
		return fmt.Errorf("update.strategy: %s", unknownValue("strategy", c.Update.Strategy, []string{StrategyRolling, StrategyParallel}))
	}
	if !slices.Contains(logLevels, c.Log.Level) {
		return fmt.Errorf("log.level: %s", unknownValue("level", c.Log.Level, []string{"debug", "info", "warn", "error"}))
	}
	if !slices.Contains(logFormats, c.Log.Format) {
		return fmt.Errorf("log.format: %s", unknownValue("format", c.Log.Format, logFormats))
	}
	if c.Schedule.Cron != "" && c.Schedule.Interval > 0 {
		return errors.New("schedule: set either cron or interval, not both")
	}
	if c.HTTP.UpdateAPI && c.HTTP.Token == "" {
		return errors.New("http.update_api: requires http.token — an unauthenticated trigger would let anyone on the network restart containers")
	}
	if c.HTTP.ReadToken != "" && c.HTTP.ReadToken == c.HTTP.Token {
		return errors.New("http.read_token: must differ from http.token — the read token would also trigger updates")
	}
	if err := validateInstance(c.Instance); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	if c.State.Dir != "" && !filepath.IsAbs(c.State.Dir) {
		return fmt.Errorf("state.dir: %q must be an absolute path (the default is %s)", c.State.Dir, DefaultStateDir)
	}
	if c.Schedule.Cron != "" {
		if _, err := ParseCron(c.Schedule.CronSpec()); err != nil {
			return fmt.Errorf("schedule.cron: %w", err)
		}
	}
	if c.Schedule.Interval < 0 {
		return errors.New("schedule.interval: must not be negative")
	}
	if c.Update.StopTimeout < 0 {
		return errors.New("update.stop_timeout: must not be negative")
	}
	if c.Hooks.Timeout < 0 {
		return errors.New("hooks.timeout: must not be negative")
	}
	if c.Audit.Retention < 0 {
		return errors.New("audit.retention: must not be negative")
	}
	if err := validateWindows("schedule", c.Schedule.Timezone, c.Schedule.Windows); err != nil {
		return err
	}
	if a := c.Update.Approval; a != nil {
		if a.URL != "" && !isHTTPURL(a.URL) {
			return errors.New("update.approval.url: must be an http:// or https:// URL with a host")
		}
		if a.Method != "" && !slices.Contains(approvalMethods, a.Method) {
			return fmt.Errorf("update.approval.method: %s", unknownValue("method", a.Method, approvalMethods))
		}
		if a.ApproveOn != "" && !slices.Contains(approveOnValues, a.ApproveOn) {
			return fmt.Errorf("update.approval.approve_on: %s", unknownValue("approve_on", a.ApproveOn, approveOnValues))
		}
		if a.Timeout < 0 {
			return errors.New("update.approval.timeout: must not be negative")
		}
		if s := a.Signing; s != nil {
			if !approvalKeyIDRE.MatchString(s.KeyID) {
				return errors.New("update.approval.signing.key_id: 1 to 64 characters of A-Z, a-z, 0-9, '.', '_' and '-'")
			}
			if s.SecretFile == "" {
				return errors.New("update.approval.signing.secret_file: required")
			}
			for k := range a.Headers {
				for _, h := range approvalSigningHeaders {
					if http.CanonicalHeaderKey(k) == h {
						return fmt.Errorf("update.approval.headers: %s is set by signing", h)
					}
				}
			}
		}
	}
	if c.Filters.IncludeRegex != "" {
		re, err := regexp.Compile(c.Filters.IncludeRegex)
		if err != nil {
			return fmt.Errorf("filters.include_regex: %w", err)
		}
		c.Filters.includeRe = re
	}
	if c.Filters.ExcludeRegex != "" {
		re, err := regexp.Compile(c.Filters.ExcludeRegex)
		if err != nil {
			return fmt.Errorf("filters.exclude_regex: %w", err)
		}
		c.Filters.excludeRe = re
	}
	if err := c.validateNotify(); err != nil {
		return err
	}
	if err := c.validateHosts(); err != nil {
		return err
	}
	for i, r := range c.Registry {
		if r.Server == "" {
			return fmt.Errorf("registry[%d]: server is required", i)
		}
		host, path := r.Scope()
		if host == "" {
			return fmt.Errorf("registry[%d]: server %q has no host", i, r.Server)
		}
		if strings.ContainsAny(path, ":@ \t") {
			return fmt.Errorf("registry[%d]: server %q: the repository path must not contain a tag, digest or whitespace", i, r.Server)
		}
		if r.PasswordFile != "" {
			if r.Password != "" {
				return fmt.Errorf("registry[%d] (%s): set either password or password_file, not both", i, r.Server)
			}
			if r.Username == "" {
				return fmt.Errorf("registry[%d] (%s): username is required with password_file", i, r.Server)
			}
		}
	}
	if err := c.validateHooks(); err != nil {
		return err
	}
	if err := c.validateJobs(); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	defaultCount := 0
	for i, p := range c.Update.Policies {
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if IsReservedPolicyID(id) {
			return fmt.Errorf("update.policies[%d].id: %q is reserved: the label dockpaum.policy=%s leaves a container alone", i, p.ID, id)
		}
		if !ValidMode(p.Mode) {
			return fmt.Errorf("update.policies[%d] (id=%q).mode: %s", i, p.ID, unknownValue("mode", p.Mode, modes))
		}
		if _, _, err := p.Tags.Compile(); err != nil {
			return fmt.Errorf("update.policies[%d] (id=%q).tags.%w", i, p.ID, err)
		}
		if err := p.Tags.checkTransform(); err != nil {
			return fmt.Errorf("update.policies[%d] (id=%q).tags.transform: %w", i, p.ID, err)
		}
		if !ValidAgeSource(p.MinImageAgeSource) {
			return fmt.Errorf("update.policies[%d] (id=%q).min_image_age_source: %s", i, p.ID, unknownValue("source", p.MinImageAgeSource, ageSources))
		}
		if p.MinImageAge < 0 {
			return fmt.Errorf("update.policies[%d] (id=%q).min_image_age: must not be negative", i, p.ID)
		}
		if p.HealthTimeout < 0 {
			return fmt.Errorf("update.policies[%d] (id=%q).health_timeout: must not be negative", i, p.ID)
		}
		if err := validateWindows(fmt.Sprintf("update.policies[%d] (id=%q)", i, p.ID), p.Timezone, p.Windows); err != nil {
			return err
		}
		if p.Schedule != "" {
			if _, err := ParseCron(p.CronSpec()); err != nil {
				return fmt.Errorf("update.policies[%d] (id=%q).schedule: %w", i, p.ID, err)
			}
		}
		for _, ph := range []struct {
			name  string
			hooks []string
		}{{"pre_update", p.Hooks.PreUpdate}, {"post_update", p.Hooks.PostUpdate}, {"on_failure", p.Hooks.OnFailure}} {
			if err := validateExecHooks(ph.hooks, false); err != nil {
				return fmt.Errorf("update.policies[%d] (id=%q).hooks.%s: %w", i, p.ID, ph.name, err)
			}
		}
		if p.IsDefault() {
			defaultCount++
		}
		key := id
		if p.IsDefault() {
			key = "default"
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("update.policies[%d]: duplicate id %q", i, key)
		}
		seen[key] = struct{}{}
	}
	if defaultCount > 1 {
		return fmt.Errorf("update.policies: more than one default policy (id empty or \"default\")")
	}
	return nil
}

func (c *Config) validateHooks() error {
	for _, ph := range []struct {
		name  string
		hooks []string
	}{{"pre_run", c.Hooks.PreRun}, {"post_run", c.Hooks.PostRun}} {
		for _, h := range ph.hooks {
			if isExecHook(h) {
				return fmt.Errorf("hooks.%s: exec: hooks are not supported here (a run spans all hosts); use a host command", ph.name)
			}
		}
	}
	return nil
}

func (c *Config) validateJobs() error {
	hostNames := make(map[string]struct{}, len(c.Hosts))
	for _, h := range c.Hosts {
		hostNames[h.Name] = struct{}{}
	}
	seen := map[string]struct{}{}
	for i, j := range c.Jobs {
		if strings.TrimSpace(j.Name) == "" {
			return fmt.Errorf("jobs[%d]: name is required", i)
		}
		if _, dup := seen[j.Name]; dup {
			return fmt.Errorf("jobs[%d]: duplicate name %q", i, j.Name)
		}
		seen[j.Name] = struct{}{}
		if j.Cron == "" {
			return fmt.Errorf("jobs[%d] (%s): cron is required", i, j.Name)
		}
		if _, err := ParseCron(j.CronSpec()); err != nil {
			return fmt.Errorf("jobs[%d] (%s).cron: %w", i, j.Name, err)
		}
		if len(j.Commands) == 0 {
			return fmt.Errorf("jobs[%d] (%s): commands is required", i, j.Name)
		}
		if j.Timeout < 0 {
			return fmt.Errorf("jobs[%d] (%s).timeout: must not be negative", i, j.Name)
		}
		if j.Host != "" {
			if _, ok := hostNames[j.Host]; !ok {
				return fmt.Errorf("jobs[%d] (%s).host: unknown host %q", i, j.Name, j.Host)
			}
		}
		if err := validateExecHooks(j.Commands, true); err != nil {
			return fmt.Errorf("jobs[%d] (%s).commands: %w", i, j.Name, err)
		}
		if j.Host == "" && len(c.Hosts) > 1 {
			for _, cmd := range j.Commands {
				if isExecHook(cmd) {
					return fmt.Errorf("jobs[%d] (%s): host is required for exec: commands when several hosts are configured", i, j.Name)
				}
			}
		}
	}
	return nil
}

var notifyTypes = []string{"webhook", "slack", "discord", "teams", "telegram", "ntfy", "gotify", "email", "pushover", "matrix", "apprise"}

func (c *Config) validateNotify() error {
	for i, n := range c.Notify {
		label := c.notifyLabel(i)
		if n.Type == "" {
			if n.URL != "" {
				return fmt.Errorf("%s: type is required with this url (a service URL such as discord://… or ntfy://…, or a Discord, Slack or Teams workflow webhook URL needs none)", label)
			}
			return fmt.Errorf("%s: type is required", label)
		}
		switch n.Type {
		case "webhook", "slack", "discord", "ntfy", "teams":
			if n.URL == "" {
				return fmt.Errorf("%s (%s): url is required", label, n.Type)
			}
		case "telegram":
			if n.Token == "" || n.Channel == "" {
				return fmt.Errorf("%s (telegram): token (bot token) and channel (chat id) are required", label)
			}
		case "gotify":
			if n.URL == "" || n.Token == "" {
				return fmt.Errorf("%s (gotify): url and token are required", label)
			}
		case "email":
			if n.SMTP == nil || n.SMTP.Host == "" {
				return fmt.Errorf("%s (email): smtp.host is required", label)
			}
			if p := n.SMTP.Port; p < 0 || p > 65535 {
				return fmt.Errorf("%s (email).smtp.port: %d is not a port number (want 1 to 65535; default %d)", label, p, DefaultSMTPPort)
			}
			if len(n.To) == 0 {
				return fmt.Errorf("%s (email): to is required", label)
			}
		case "pushover":
			if n.Token == "" || n.User == "" {
				return fmt.Errorf("%s (pushover): token (application API token) and user (user or group key) are required", label)
			}
		case "matrix":
			if n.URL == "" || n.Token == "" || n.Room == "" {
				return fmt.Errorf("%s (matrix): url (homeserver), token (access token) and room are required", label)
			}
			if !strings.HasPrefix(n.Room, "!") && !strings.HasPrefix(n.Room, "#") {
				return fmt.Errorf("%s (matrix): room must be a room ID (!id:server) or an alias (#alias:server)", label)
			}
		case "apprise":
			if n.URL == "" {
				return fmt.Errorf("%s (apprise): url (the Apprise API notify endpoint) is required", label)
			}
			for j, t := range n.To {
				if urlScheme(t) == "" {
					return fmt.Errorf("%s (apprise).to[%d]: not an Apprise URL (want service://…)", label, j)
				}
			}
		default:
			return fmt.Errorf("%s: %s", label, unknownValue("type", n.Type, notifyTypes))
		}
		for j, e := range n.Events {
			if e != "*" && !slices.Contains(notifyEvents, e) {
				msg := unknownValue("event", e, append(slices.Clone(notifyEvents), "*"))
				if strings.ContainsAny(e, ", ") {
					msg += "; list several events as [update.failure, run.completed]"
				}
				return fmt.Errorf("%s.events[%d]: %s", label, j, msg)
			}
		}
		if n.URL != "" && n.Type != "email" && !isHTTPURL(n.URL) {
			return fmt.Errorf("%s (%s).url: must be an http:// or https:// URL, or a service URL such as discord://… with type left out", label, n.Type)
		}
	}
	return nil
}

const maxInstanceLen = 64

func validateInstance(name string) error {
	if !utf8.ValidString(name) {
		return errors.New("not valid UTF-8")
	}
	if n := utf8.RuneCountInString(name); n > maxInstanceLen {
		return fmt.Errorf("%d characters is too long for a name (at most %d)", n, maxInstanceLen)
	}
	for _, r := range name {
		if !unicode.IsGraphic(r) && r != zeroWidthNonJoiner && r != zeroWidthJoiner {
			return fmt.Errorf("%q contains %s (%U); use a short name such as nas-1", name, invisibleKind(r), r)
		}
	}
	return nil
}

const (
	zeroWidthNonJoiner = '\u200c'
	zeroWidthJoiner    = '\u200d'
)

func invisibleKind(r rune) string {
	switch {
	case r == '\n' || r == '\r' || r == '\u0085' || r == '\u2028' || r == '\u2029':
		return "a line break"
	case r == '\t':
		return "a tab"
	case unicode.IsControl(r):
		return "a control character"
	case unicode.Is(unicode.Cf, r):
		return "an invisible formatting character"
	}
	return "a character that is not printable"
}

var approvalMethods = []string{"POST", "PUT", "PATCH", "GET", "HEAD", "DELETE"}

func unknownValue(what, value string, allowed []string) string {
	msg := fmt.Sprintf("unknown %s %q", what, value)
	if s := suggest(value, allowed); s != "" {
		msg += fmt.Sprintf(" — did you mean %q?", s)
	}
	return msg + " (want " + orList(allowed) + ")"
}

func orList(values []string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (c *Config) validateHosts() error {
	first := map[string]int{}
	for i, h := range c.Hosts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Endpoint)), "ssh://") {
			return fmt.Errorf("hosts[%d] (%s).endpoint: ssh:// is not supported; use a local socket (unix://) or tcp:// with TLS", i, h.Name)
		}
		if j, dup := first[h.Name]; dup {
			return fmt.Errorf("hosts[%d].name: %q is already the name of hosts[%d]; give every host its own name", i, h.Name, j)
		}
		first[h.Name] = i
	}
	return nil
}

func isExecHook(h string) bool {
	return strings.HasPrefix(strings.TrimSpace(h), "exec:")
}

func validateExecHooks(hooks []string, needContainer bool) error {
	for _, h := range hooks {
		if !isExecHook(h) {
			continue
		}
		container, cmd, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(h), "exec:"), ":")
		if !ok || strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("invalid exec hook %q (want exec:<container>:<command>)", h)
		}
		if c := strings.TrimSpace(container); needContainer && (c == "" || c == "@self") {
			return fmt.Errorf("exec hook %q must name a container", h)
		}
	}
	return nil
}

func validateWindows(base, tz string, specs []WindowSpec) error {
	if len(specs) == 0 {
		return nil
	}
	ws := make([]window.Spec, len(specs))
	commas := false
	for i, s := range specs {
		ws[i] = window.Spec{Days: s.Days, Times: s.Times}
		for _, v := range append(slices.Clone(s.Days), s.Times...) {
			commas = commas || strings.Contains(v, ",")
		}
	}
	if _, err := window.Compile(tz, ws); err != nil {
		msg := fmt.Sprintf("%s.%v", base, err)
		if commas {
			msg += "; list several as [Mon, Tue]"
		}
		return errors.New(msg)
	}
	return nil
}

const redactedValue = "***"

func (c *Config) Redacted() *Config {
	out := *c
	effective := func(v bool) *bool { return &v }
	out.HTTP.Enabled = effective(c.HTTP.IsEnabled())
	out.Filters.WatchtowerLabels = effective(c.Filters.HonorWatchtowerLabels())
	out.Update.RollbackOnFail = effective(c.Update.Rollback())
	out.Update.SelfUpdate.Enabled = effective(c.Update.SelfUpdate.IsEnabled())
	out.Audit.Enabled = effective(c.Audit.IsEnabled())
	out.State.Enabled = effective(c.State.IsEnabled())
	out.Schedule.RunOnStart = effective(c.RunOnStart())

	out.Registry = append([]RegistryAuth(nil), c.Registry...)
	for i := range out.Registry {
		if out.Registry[i].Password != "" {
			out.Registry[i].Password = redactedValue
		}
	}

	out.Notify = append([]NotifierConfig(nil), c.Notify...)
	for i := range out.Notify {
		n := &out.Notify[i]
		n.Once = effective(n.ReportsOnce())
		if n.Token != "" {
			n.Token = redactedValue
		}
		if n.User != "" {
			n.User = redactedValue
		}
		if n.Type == "apprise" && len(n.To) > 0 {
			to := make([]string, len(n.To))
			for j, t := range n.To {
				to[j] = maskServiceURL(t)
			}
			n.To = to
		}
		n.URL = maskURL(n.URL)
		n.Headers = redactMapValues(n.Headers)
		if n.SMTP != nil {
			smtp := *n.SMTP
			if smtp.Password != "" {
				smtp.Password = redactedValue
			}
			n.SMTP = &smtp
		}
	}

	if c.Update.Approval != nil {
		a := *c.Update.Approval
		a.Headers = redactMapValues(a.Headers)
		a.URL = maskURL(a.URL)
		if a.Signing != nil {
			s := *a.Signing
			a.Signing = &s
		}
		out.Update.Approval = &a
	}

	if c.HTTP.Token != "" {
		out.HTTP.Token = redactedValue
	}
	if c.HTTP.ReadToken != "" {
		out.HTTP.ReadToken = redactedValue
	}
	return &out
}

func redactMapValues(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k := range in {
		out[k] = redactedValue
	}
	return out
}

func maskServiceURL(raw string) string {
	if s := urlScheme(raw); s != "" {
		return s + "://" + redactedValue
	}
	return redactedValue
}

func maskURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return redactedValue
	}
	return u.Scheme + "://" + u.Host + "/" + redactedValue
}
