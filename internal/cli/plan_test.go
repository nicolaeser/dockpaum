package cli

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/selfupdate"
	"github.com/nicolaeser/dockpaum/internal/updater"
	"github.com/nicolaeser/dockpaum/internal/window"
)

const unreachableRegistry = "127.0.0.1:1/"

type planFixture struct {
	d            *dockertest.Daemon
	reg          *testRegistry
	config, body string
	stateDir     string
	auditDir     string
	tags         map[string]string
}

func (f *planFixture) publish(repo, tag string, img registryImage) (string, dockertest.Image) {
	manifest, id := f.reg.push(repo, tag, img)
	ref := f.reg.host() + "/" + repo + ":" + tag
	return ref, dockertest.Image{
		ID: id, Architecture: "amd64", Created: img.created.UTC().Format(time.RFC3339),
		RepoDigests: []string{f.reg.host() + "/" + repo + "@" + manifest},
		Config:      &container.Config{Labels: img.labels},
	}
}

func (f *planFixture) local(repo, tag string, running registryImage, newer *registryImage) string {
	ref, img := f.publish(repo, tag, running)
	f.d.AddImage(img, ref)
	if newer != nil {
		_, img = f.publish(repo, tag, *newer)
	}
	f.d.Publish(ref, img)
	f.tags[ref] = f.d.ImageID(ref)
	return ref
}

func newPlanFixture(t *testing.T, dryRun bool) *planFixture {
	t.Helper()
	clearConfigEnv(t)
	f := &planFixture{d: dockertest.New(t, "1.47"), reg: newTestRegistry(t), tags: map[string]string{}}
	now := time.Now().UTC().Truncate(time.Second)
	old, fresh := now.Add(-30*24*time.Hour), now.Add(-time.Hour)
	img := func(build string, created time.Time, version string) registryImage {
		labels := map[string]string{}
		if version != "" {
			labels[ociVersionLabel] = version
		}
		return registryImage{build: build, created: created, labels: labels}
	}
	newer := func(i registryImage) *registryImage { return &i }
	run := func(name, ref string, labels map[string]string) {
		f.d.Run(name, &container.Config{Image: ref, Labels: labels}, &container.HostConfig{}, nil)
	}

	run("web", f.local("web", "latest", img("a1", old, "1.27.1"), newer(img("a2", old, "1.27.2"))), nil)
	run("db", f.local("db", "16", img("b1", old, ""), newer(img("b2", fresh, ""))), map[string]string{policy.LabelPolicy: "database"})
	run("cache", f.local("cache", "7", img("c1", old, ""), nil), nil)
	run("legacy", f.local("legacy", "1", img("d1", old, ""), newer(img("d2", old, ""))), map[string]string{policy.LabelPolicy: "none"})
	run("watched", f.local("watched", "2", img("e1", old, ""), newer(img("e2", old, ""))), map[string]string{policy.LabelMonitorOnly: "true"})
	run("batch", f.local("batch", "3", img("f1", old, ""), newer(img("f2", old, ""))), map[string]string{policy.LabelPolicy: "weekly"})
	run("ignored", f.local("ignored", "1", img("g1", old, ""), nil), map[string]string{policy.LabelEnable: "false"})
	stopped := f.local("stopped", "1", img("h1", old, ""), nil)
	f.d.Create("stopped", &container.Config{Image: stopped}, &container.HostConfig{})

	windowDay := now.AddDate(0, 0, 2).Weekday().String()[:3]
	f.stateDir = filepath.Join(t.TempDir(), "state")
	f.auditDir = filepath.Join(t.TempDir(), "audit")
	body := "dry_run: " + map[bool]string{true: "true", false: "false"}[dryRun] + "\n" +
		"timezone: UTC\n" +
		"schedule: {cron: \"0 4 * * *\"}\n" +
		"hosts: [{name: local, endpoint: \"" + f.d.Endpoint() + "\"}]\n" +
		"http: {enabled: false}\n" +
		"audit: {enabled: true, dir: \"" + f.auditDir + "\"}\n" +
		"state: {dir: \"" + f.stateDir + "\"}\n" +
		"update:\n" +
		"  policies:\n" +
		"    - {id: default, mode: tag-follow, min_image_age: 0s}\n" +
		"    - {id: database, mode: tag-follow, min_image_age: 30d}\n" +
		"    - id: weekly\n" +
		"      mode: tag-follow\n" +
		"      windows: [{days: [" + windowDay + "], times: [\"00:00-23:59\"]}]\n"
	f.config, f.body = writeConfig(t, body), body
	return f
}

func (f *planFixture) mutations() []string {
	var out []string
	for _, suffix := range []string{"/start", "/stop", "/restart", "/rename", "/create", "/kill"} {
		for _, c := range f.d.Calls("POST", suffix) {
			if strings.HasPrefix(c.Path, "/containers/") {
				out = append(out, c.Method+" "+c.Path)
			}
		}
	}
	for _, c := range f.d.Calls("DELETE", "") {
		if strings.HasPrefix(c.Path, "/containers/") || strings.HasPrefix(c.Path, "/images/") {
			out = append(out, c.Method+" "+c.Path)
		}
	}
	return out
}

func (f *planFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	if m := f.mutations(); len(m) > 0 {
		t.Errorf("the evaluation changed containers or images: %v", m)
	}
	for ref, id := range f.tags {
		if got := f.d.ImageID(ref); got != id {
			t.Errorf("%s names %s now, %s before", ref, got, id)
		}
	}
	if f.reg.count(http.MethodHead, "/manifests/") > 0 {
		pulls, tags := f.d.Calls("POST", "/images/create"), f.d.Calls("POST", "/tag")
		if len(pulls) > 0 || len(tags) > 0 {
			t.Errorf("the registry answered, yet the evaluation pulled %d and tagged %d image(s)", len(pulls), len(tags))
		}
	}
	for _, dir := range []string{f.stateDir, f.auditDir} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("the evaluation wrote %s: %v", dir, entries)
		}
	}
}

func decodePlan(t *testing.T, out string) planDoc {
	t.Helper()
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("plan --json is not JSON: %v\n%s", err, out)
	}
	return doc
}

func TestPlanReportsEveryOutcomeAndChangesNothing(t *testing.T) {
	f := newPlanFixture(t, false)
	code, out, errOut := execute(t, "plan", "--json", "--config", f.config)
	if code != exitOK {
		t.Fatalf("plan: exit %d\n%s\n%s", code, out, errOut)
	}
	doc := decodePlan(t, out)
	if doc.DryRun || doc.Schedule != "cron 0 4 * * * (UTC)" || doc.Instance == "" {
		t.Errorf("plan header = %+v", doc)
	}
	got := map[string]planEntry{}
	for _, e := range doc.Containers {
		got[e.Container] = e
	}
	want := map[string]struct{ action, decision, policy, source string }{
		"web":     {actionUpdate, "dry-run", "default", "default"},
		"db":      {actionWait, "gated-min-age", "database", "label"},
		"cache":   {actionNone, "skipped", "default", "default"},
		"legacy":  {actionHold, "gated-policy", "none", "label"},
		"watched": {actionReport, "monitor-only", "default", "default"},
		"batch":   {actionWait, "gated-window", "weekly", "label"},
	}
	for name, w := range want {
		e, ok := got[name]
		if !ok {
			t.Errorf("%s is not in the plan: %+v", name, doc.Containers)
			continue
		}
		if e.Action != w.action || e.Decision != w.decision || e.Policy != w.policy || e.PolicySource != w.source {
			t.Errorf("%s = action %q decision %q policy %q (%s), want %q %q %q (%s)",
				name, e.Action, e.Decision, e.Policy, e.PolicySource, w.action, w.decision, w.policy, w.source)
		}
	}
	if len(got) != len(want) {
		t.Errorf("plan lists %d containers, want %d", len(got), len(want))
	}

	web := got["web"]
	if web.Candidate == nil || web.Current.Version != "1.27.1" || web.Candidate.Version != "1.27.2" ||
		web.Current.Tag != "latest" || web.NextRun == nil || web.NextRun.UTC().Hour() != 4 {
		t.Errorf("web = %+v (candidate %+v)", web, web.Candidate)
	}
	if web.Reason != "newer image available" {
		t.Errorf("web reason = %q", web.Reason)
	}
	db := got["db"]
	if db.EligibleAt == nil || db.EligibleAt.Before(time.Now().Add(28*24*time.Hour)) || db.NextRun == nil || db.NextRun.Before(*db.EligibleAt) {
		t.Errorf("db eligible %v next run %v", db.EligibleAt, db.NextRun)
	}
	batch := got["batch"]
	if batch.NextWindow == nil || batch.NextWindow.Weekday() != time.Now().UTC().AddDate(0, 0, 2).Weekday() {
		t.Errorf("batch next window = %v", batch.NextWindow)
	}

	var unmanaged []string
	for _, u := range doc.Unmanaged {
		unmanaged = append(unmanaged, u.Container+": "+u.Reason)
	}
	if !slices.Contains(unmanaged, "ignored: labelled dockpaum.enable=false") ||
		!slices.Contains(unmanaged, "stopped: not running (created) and filters.include_stopped is off") || len(unmanaged) != 2 {
		t.Errorf("unmanaged = %q", unmanaged)
	}

	f.assertUnchanged(t)
}

func TestPlanTable(t *testing.T) {
	f := newPlanFixture(t, true)
	code, out, errOut := execute(t, "plan", "--config", f.config)
	if code != exitOK {
		t.Fatalf("plan: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("a table written to a non-terminal must not carry color")
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "dry_run: reports updates, changes nothing") {
		t.Errorf("header line = %q", lines[0])
	}
	header := lines[2]
	for _, col := range []string{"HOST", "CONTAINER", "POLICY", "CURRENT -> CANDIDATE", "PLAN", "WHEN", "REASON"} {
		if !strings.Contains(header, col) {
			t.Errorf("header %q lacks %s", header, col)
		}
	}
	at := strings.Index(header, "PLAN")
	var web, db string
	for _, l := range lines[3:] {
		if strings.HasPrefix(l, "local  web ") {
			web = l
		}
		if strings.HasPrefix(l, "local  db ") {
			db = l
		}
	}
	if !strings.HasPrefix(web[at:], "report") || !strings.HasPrefix(db[at:], "wait") {
		t.Errorf("rows not aligned under PLAN (%d):\n%s", at, out)
	}
	for _, want := range []string{
		"latest (1.27.1) ", " -> (1.27.2) ", "database (label)",
		"newer image available; dry_run is on, so it is reported, not applied",
		"2 other containers are not managed by this instance (filters)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
}

func TestPlanNarrowedByContainerAndPolicy(t *testing.T) {
	f := newPlanFixture(t, false)
	code, out, errOut := execute(t, "plan", "--json", "--config", f.config, "--container", "db", "--container", "/ignored")
	if code != exitOK {
		t.Fatalf("plan --container: exit %d\n%s\n%s", code, out, errOut)
	}
	doc := decodePlan(t, out)
	if len(doc.Containers) != 1 || doc.Containers[0].Container != "db" {
		t.Errorf("containers = %+v, want db only", doc.Containers)
	}
	if len(doc.Unmanaged) != 1 || doc.Unmanaged[0].Container != "ignored" {
		t.Errorf("unmanaged = %+v, want the requested ignored", doc.Unmanaged)
	}

	code, out, _ = execute(t, "plan", "--json", "--config", f.config, "--policy", "Database")
	if code != exitOK {
		t.Fatalf("plan --policy: exit %d", code)
	}
	if doc := decodePlan(t, out); len(doc.Containers) != 1 || doc.Containers[0].Container != "db" || len(doc.Unmanaged) != 0 {
		t.Errorf("plan --policy database = %+v", doc)
	}

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--policy", "databse"}, `unknown policy — did you mean "database"?`},
		{[]string{"--policy", "none"}, "never checked"},
		{[]string{"--container", "dbb"}, `no container named "dbb" on host local — did you mean "db"?`},
	}
	for _, c := range cases {
		code, _, errOut := execute(t, append([]string{"plan", "--config", f.config}, c.args...)...)
		if code != exitUsage || !strings.Contains(errOut, c.want) {
			t.Errorf("plan %q: exit %d, stderr %q, want 2 and %q", c.args, code, errOut, c.want)
		}
	}
	f.assertUnchanged(t)
}

func TestPlanStopsBeforeChangingLeftovers(t *testing.T) {
	f := newPlanFixture(t, false)
	f.d.AddImage(dockertest.Image{ID: "z1"}, unreachableRegistry+"app:1")
	f.d.Run("app.dockpaum-old-1700000000000000000", &container.Config{Image: unreachableRegistry + "app:1"}, &container.HostConfig{}, nil)
	code, out, errOut := execute(t, "plan", "--config", f.config)
	if code != exitFailure || out != "" {
		t.Fatalf("plan with a stranded container: exit %d\n%s", code, out)
	}
	if !strings.Contains(errOut, "app.dockpaum-old-1700000000000000000 is left from an interrupted update") ||
		!strings.Contains(errOut, "changes nothing, so it stops here") {
		t.Errorf("stderr = %q", errOut)
	}
	f.assertUnchanged(t)
}

func TestExplain(t *testing.T) {
	f := newPlanFixture(t, false)
	code, out, errOut := execute(t, "explain", "db", "--config", f.config)
	if code != exitOK {
		t.Fatalf("explain db: exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"db on local", "policy     database (label)", "Gates, in the order the daemon applies them:",
		"label dockpaum.policy=database", "What would change it:", `lower min_image_age of policy "database"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain db lacks %q:\n%s", want, out)
		}
	}

	code, out, _ = execute(t, "explain", "--json", "--config", f.config, "db")
	if code != exitOK {
		t.Fatalf("explain --json: exit %d", code)
	}
	var doc explainDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Containers) != 1 {
		t.Fatalf("explain --json = %v\n%s", err, out)
	}
	verdicts := map[string]string{}
	var order []string
	for _, g := range doc.Containers[0].Gates {
		verdicts[g.Gate] = g.Verdict
		order = append(order, g.Gate)
	}
	wantOrder := []string{"filters", "policy", "container", "mode", "digest pin", "registry", "window",
		"soak", "monitor-only", "dry-run", "approval", "update"}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("gates %q, want %q", order, wantOrder)
	}
	for g, v := range map[string]string{
		gateFilters: verdictPass, gatePolicy: verdictPass, gateWindow: verdictPass, gateRegistry: verdictPass,
		gateSoak: verdictStop, gateMonitor: verdictNotReached, gateApproval: verdictNotReached,
	} {
		if verdicts[g] != v {
			t.Errorf("gate %s = %q, want %q", g, verdicts[g], v)
		}
	}

	code, out, _ = execute(t, "explain", "--json", "--config", f.config, "ignored")
	if code != exitOK {
		t.Fatalf("explain ignored: exit %d", code)
	}
	doc = explainDoc{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Containers) != 1 {
		t.Fatalf("explain ignored = %v\n%s", err, out)
	}
	g := doc.Containers[0].Gates[0]
	if doc.Containers[0].Managed || g.Gate != gateFilters || g.Verdict != verdictStop || g.Change != "remove the label, or set it to true" {
		t.Errorf("ignored = %+v", doc.Containers[0])
	}

	code, out, _ = execute(t, "explain", "batch", "--config", f.config)
	if code != exitOK || !strings.Contains(out, "window        stop") || !strings.Contains(out, `change the windows of policy "weekly"`) {
		t.Errorf("explain batch: exit %d\n%s", code, out)
	}
	code, out, _ = execute(t, "explain", "batch", "--json", "--config", f.config)
	doc = explainDoc{}
	if err := json.Unmarshal([]byte(out), &doc); code != exitOK || err != nil || len(doc.Containers) != 1 {
		t.Fatalf("explain batch --json: exit %d, %v\n%s", code, err, out)
	}
	batch := doc.Containers[0]
	verdicts = map[string]string{}
	for _, g := range batch.Gates {
		verdicts[g.Gate] = g.Verdict
	}
	wantRegistry := verdictNotReached
	if batch.Candidate != nil {
		wantRegistry = verdictPass
	}
	if batch.Action != actionWait || verdicts[gateWindow] != verdictStop || verdicts[gateRegistry] != wantRegistry ||
		verdicts[gateSoak] != verdictNotReached || batch.NextWindow == nil {
		t.Errorf("explain batch = %+v", batch)
	}
	if code, _, errOut := execute(t, "explain", "nope", "--config", f.config); code != exitUsage || !strings.Contains(errOut, `no container named "nope"`) {
		t.Errorf("explain nope: exit %d %q", code, errOut)
	}
	f.assertUnchanged(t)
}

func TestExplainDryRunGate(t *testing.T) {
	f := newPlanFixture(t, true)
	code, out, errOut := execute(t, "explain", "web", "--config", f.config)
	if code != exitOK {
		t.Fatalf("explain web: exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"dry-run       stop", "approval      not reached", "dry-run: set dry_run: false",
		"current    latest (1.27.1) ", "candidate  (1.27.2) ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain web lacks %q:\n%s", want, out)
		}
	}
}

func TestPlanReportsAContainerItCannotCheck(t *testing.T) {
	f := newPlanFixture(t, false)
	ref := unreachableRegistry + "ghost:1"
	f.d.AddImage(dockertest.Image{ID: "sha256:" + strings.Repeat("9", 64), Architecture: "amd64",
		RepoDigests: []string{strings.TrimSuffix(unreachableRegistry, "/") + "/ghost@sha256:" + strings.Repeat("8", 64)}}, ref)
	f.tags[ref] = f.d.ImageID(ref)
	f.d.Run("ghost", &container.Config{Image: ref}, &container.HostConfig{}, nil)

	code, out, errOut := execute(t, "plan", "--json", "--config", f.config)
	if code != exitFailure || !strings.Contains(errOut, "error: 1 container could not be checked") {
		t.Fatalf("plan: exit %d, want 1\n%s", code, errOut)
	}
	got := map[string]planEntry{}
	for _, e := range decodePlan(t, out).Containers {
		got[e.Container] = e
	}
	if g := got["ghost"]; g.Action != actionError || g.Candidate != nil || g.Reason == "" {
		t.Errorf("ghost = %+v", g)
	}
	if got["web"].Action != actionUpdate {
		t.Errorf("one container that cannot be checked spoils the others: web = %+v", got["web"])
	}
	f.assertUnchanged(t)
}

func TestConnectLog(t *testing.T) {
	var buf strings.Builder
	h := newConnectLog(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	l := slog.New(h)
	l.Error("docker connect failed, host will be retried each run", "host", "a", "err", "dial unix /a.sock: no such file")
	l.Warn("docker reconnect failed", "host", "a", "err", "still gone")
	l.Warn("TLS certificate verification is DISABLED for this host", "host", "b", "endpoint", "tcp://b:2376")
	l.Info("docker connected", "host", "b")
	if out := buf.String(); strings.Contains(out, "failed") || !strings.Contains(out, "DISABLED") || strings.Contains(out, "connected") {
		t.Errorf("passed on: %q", out)
	}
	if got := h.failure("a"); got != "still gone" {
		t.Errorf("failure(a) = %q", got)
	}
	if got := h.failure("b"); got != "no connection" {
		t.Errorf("failure(b) = %q", got)
	}
	hosts := []config.HostConfig{{Name: "b"}, {Name: "a"}}
	if got := h.failures(hosts, errors.New("host b: x")); !slices.Equal(got, []string{"host a: still gone"}) {
		t.Errorf("failures = %q", got)
	}
	if got := newConnectLog(slog.DiscardHandler).failures(hosts, errors.New("host b: x")); !slices.Equal(got, []string{"host b: x"}) {
		t.Errorf("failures without records = %q", got)
	}
}

func TestPlanWithAnUnreachableHost(t *testing.T) {
	f := newPlanFixture(t, false)
	sock := filepath.Join(t.TempDir(), "gone.sock")
	local := "hosts: [{name: local, endpoint: \"" + f.d.Endpoint() + "\"}]"
	cfg := writeConfig(t, strings.Replace(f.body, local,
		"hosts: [{name: local, endpoint: \""+f.d.Endpoint()+"\"}, {name: gone, endpoint: \"unix://"+sock+"\"}]", 1))
	code, out, errOut := execute(t, "plan", "--config", cfg)
	if code != exitFailure || !strings.Contains(out, "local  web ") {
		t.Fatalf("plan: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Count(errOut, "unix://"+sock) != 1 || !strings.Contains(errOut, "error: host gone: unreachable (") ||
		strings.Contains(errOut, "retried each run") || strings.Contains(errOut, "reconnect failed") {
		t.Errorf("stderr = %q", errOut)
	}
	f.assertUnchanged(t)
}

func TestPlanWithoutDocker(t *testing.T) {
	clearConfigEnv(t)
	sock := filepath.Join(t.TempDir(), "missing.sock")
	cfg := writeConfig(t, "hosts: [{name: local, endpoint: \"unix://"+sock+"\"}]\nhttp: {enabled: false}\n")
	code, out, errOut := execute(t, "plan", "--config", cfg)
	if code != exitFailure || out != "" || !strings.HasPrefix(errOut, "plan: docker: host local: ") ||
		strings.Count(errOut, "unix://"+sock) != 1 || strings.Contains(errOut, "retried each run") {
		t.Errorf("plan without Docker: exit %d\n%s\n%s", code, out, errOut)
	}
	bad := writeConfig(t, "schedule: {cron: \"nonsense\"}\n")
	if code, _, errOut := execute(t, "explain", "web", "--config", bad); code != exitUsage || !strings.Contains(errOut, "schedule.cron") {
		t.Errorf("explain with a bad config: exit %d %q", code, errOut)
	}
}

func TestPlanBlockers(t *testing.T) {
	helper := map[string]string{selfupdate.HelperLabel: "abc"}
	check := map[string]string{selfupdate.CheckLabel: "1"}
	scoped := map[string]string{selfupdate.HelperLabel: "abc", policy.LabelScope: "other"}
	cases := []struct {
		name string
		cts  []docker.Container
		want int
	}{
		{"nothing", []docker.Container{{Name: "web", State: "running"}}, 0},
		{"leftover with a free name", []docker.Container{{Name: "web.dockpaum-old-1", State: "exited"}}, 1},
		{"leftover beside its replacement", []docker.Container{{Name: "web.dockpaum-old-1"}, {Name: "web", State: "running"}}, 0},
		{"self-update leftover while the helper runs", []docker.Container{
			{Name: "dockpaum.self-update-old-1"}, {Name: "helper", State: "running", Labels: helper}}, 0},
		{"self-update leftover after the helper", []docker.Container{
			{Name: "dockpaum.self-update-old-1"}, {Name: "helper", State: "exited", Labels: helper}}, 2},
		{"finished check", []docker.Container{{Name: "check", State: "exited", Labels: check}}, 1},
		{"helper of another scope", []docker.Container{{Name: "helper", State: "exited", Labels: scoped}}, 0},
		{"leftover of another scope", []docker.Container{
			{Name: "web.dockpaum-old-1", Labels: map[string]string{policy.LabelScope: "other"}}}, 0},
	}
	for _, c := range cases {
		got := planBlockers([]hostContainers{{host: "local", containers: c.cts}}, config.FilterConfig{})
		if len(got) != c.want {
			t.Errorf("%s: %d blockers %q, want %d", c.name, len(got), got, c.want)
		}
	}
}

func TestFilterVerdict(t *testing.T) {
	no := false
	cases := []struct {
		f    config.FilterConfig
		ct   docker.Container
		want string
	}{
		{config.FilterConfig{}, docker.Container{Name: "web", State: "running"}, ""},
		{config.FilterConfig{}, docker.Container{Name: "web", State: "exited"}, "not running (exited) and filters.include_stopped is off"},
		{config.FilterConfig{IncludeStopped: true}, docker.Container{Name: "web", State: "exited"}, ""},
		{config.FilterConfig{}, docker.Container{Name: "web.dockpaum-old-12", State: "running"}, "temporary name of an interrupted update; never updated itself"},
		{config.FilterConfig{LabelEnable: true}, docker.Container{Name: "web", State: "running"}, "filters.label_enable is on and the container is not labelled dockpaum.enable=true"},
		{config.FilterConfig{}, docker.Container{Name: "web", State: "running", Labels: map[string]string{"com.centurylinklabs.watchtower.enable": "false"}},
			"labelled com.centurylinklabs.watchtower.enable=false"},
		{config.FilterConfig{WatchtowerLabels: &no}, docker.Container{Name: "web", State: "running", Labels: map[string]string{"com.centurylinklabs.watchtower.enable": "false"}}, ""},
		{config.FilterConfig{Scope: "prod"}, docker.Container{Name: "web", State: "running"}, `no dockpaum.scope label, and this instance manages scope "prod" only`},
		{config.FilterConfig{}, docker.Container{Name: "web", State: "running", Labels: map[string]string{policy.LabelScope: "prod"}},
			"labelled dockpaum.scope=prod: the instance with that filters.scope manages it"},
		{config.FilterConfig{IncludeNames: []string{"db"}}, docker.Container{Name: "web", State: "running"}, "not in filters.include_names"},
		{config.FilterConfig{ExcludeNames: []string{"web"}}, docker.Container{Name: "web", State: "running"}, "in filters.exclude_names"},
	}
	for _, c := range cases {
		if got := filterVerdict(c.ct, &c.f); got != c.want {
			t.Errorf("filterVerdict(%+v, %+v) = %q, want %q", c.ct, c.f, got, c.want)
		}
	}
}

func TestPlanClock(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Schedule: config.ScheduleConfig{Cron: "0 4 * * *", Timezone: "Europe/Berlin",
			Windows: []config.WindowSpec{{Days: []string{"Sat", "Sun"}, Times: []string{"00:00-23:59"}}}},
		Update: config.UpdatePolicy{Policies: []config.Policy{
			{ID: "default", Mode: config.ModeTagFollow},
			{ID: "weekly", Mode: config.ModeTagFollow, Schedule: "30 3 * * Sun", Timezone: "Europe/Berlin"},
		}},
	}
	r, err := policy.NewResolver(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := newPlanClock(cfg, r)
	from := time.Date(2026, 10, 14, 12, 0, 0, 0, berlin)
	if got, ok := c.nextRun(from, "default"); !ok || !got.Equal(time.Date(2026, 10, 17, 4, 0, 0, 0, berlin)) {
		t.Errorf("regular next run = %v %v", got, ok)
	}
	if got, ok := c.nextRun(from, "weekly"); !ok || !got.Equal(time.Date(2026, 10, 18, 3, 30, 0, 0, berlin)) {
		t.Errorf("policy next run = %v %v", got, ok)
	}
	never, err := window.Compile("Europe/Berlin", []window.Spec{{Days: []string{"Mon"}, Times: []string{"10:00-11:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.nextRun(from, "default", never); ok {
		t.Errorf("next run with a disjoint window = %v", got)
	}
	if _, ok := newPlanClock(&config.Config{}, r).nextRun(from, "default"); ok {
		t.Error("interval schedule must not report an exact run")
	}
	every := &config.Config{Schedule: config.ScheduleConfig{Cron: "@every 6h"}}
	if c := newPlanClock(every, r); c.regular != nil || c.interval != 6*time.Hour {
		t.Errorf("@every clock = %+v", c)
	}

	if got, ok := nextOpening(from, never); !ok || !got.Equal(time.Date(2026, 10, 19, 10, 0, 0, 0, berlin)) {
		t.Errorf("next opening = %v %v", got, ok)
	}
	if got, ok := nextOpening(from, nil); !ok || !got.Equal(from) {
		t.Errorf("next opening without windows = %v %v", got, ok)
	}
}

func TestStopGate(t *testing.T) {
	cases := []struct {
		decision string
		update   string
		r        resolved
		want     string
	}{
		{"dry-run", updater.UpdateAvailable, resolved{}, ""},
		{"gated-policy", updater.UpdateUnknown, resolved{Resolved: policy.Resolved{Skip: true}}, gatePolicy},
		{"gated-policy", updater.UpdateUnknown, resolved{Resolved: policy.Resolved{Mode: config.ModeNone}}, gateMode},
		{"gated-policy", updater.UpdateUnknown, resolved{}, gateContainer},
		{"gated-policy", updater.UpdateAvailable, resolved{}, gateSelfUpdate},
		{"gated-orchestrated", updater.UpdateUnknown, resolved{}, gateContainer},
		{"gated-mac-address", updater.UpdateUnknown, resolved{}, gateContainer},
		{"gated-window", updater.UpdateUnknown, resolved{}, gateWindow},
		{"gated-window", updater.UpdateAvailable, resolved{}, gateWindow},
		{"gated-window", updater.UpdateNone, resolved{}, gateRegistry},
		{"gated-digest-pin", updater.UpdateUnknown, resolved{}, gatePin},
		{"skipped", updater.UpdateNone, resolved{}, gateRegistry},
		{"check-failed", updater.UpdateUnknown, resolved{}, gateRegistry},
		{"gated-min-age", updater.UpdateAvailable, resolved{}, gateSoak},
		{"monitor-only", updater.UpdateAvailable, resolved{}, gateMonitor},
		{"approval-denied", updater.UpdateAvailable, resolved{}, gateApproval},
	}
	for _, c := range cases {
		s := updater.ContainerStatus{Decision: c.decision, Update: c.update}
		if got := stopGate(s, c.r); got != c.want {
			t.Errorf("stopGate(%s, %s) = %q, want %q", c.decision, c.update, got, c.want)
		}
	}
}

func TestPlanAction(t *testing.T) {
	ev := &evaluation{cfg: &config.Config{}}
	observe := &evaluation{cfg: &config.Config{DryRun: true}}
	const unasked = "the registry could not be reached"
	cases := []struct {
		ev        *evaluation
		decision  string
		update    string
		checkNote string
		want      string
	}{
		{ev, "dry-run", updater.UpdateAvailable, "", actionUpdate},
		{observe, "dry-run", updater.UpdateAvailable, "", actionReport},
		{ev, "monitor-only", updater.UpdateAvailable, "", actionReport},
		{ev, "gated-min-age", updater.UpdateAvailable, "", actionWait},
		{ev, "gated-window", updater.UpdateAvailable, "", actionWait},
		{ev, "gated-window", updater.UpdateUnknown, "", actionWait},
		{ev, "gated-window", updater.UpdateNone, "", actionNone},
		{ev, "gated-policy", updater.UpdateUnknown, "", actionHold},
		{ev, "gated-digest-pin", updater.UpdateUnknown, "", actionHold},
		{ev, "gated-autoremove", updater.UpdateUnknown, "", actionHold},
		{ev, "skipped", updater.UpdateNone, "", actionNone},
		{ev, "check-failed", updater.UpdateUnknown, "", actionError},
		{ev, "failed", updater.UpdateAvailable, "", actionError},
		{ev, "check-incomplete", updater.UpdateUnknown, "", actionUnknown},
		{ev, "check-incomplete", updater.UpdateUnknown, unasked, actionError},
	}
	for _, c := range cases {
		if got := c.ev.action(updater.ContainerStatus{Decision: c.decision, Update: c.update}, c.checkNote); got != c.want {
			t.Errorf("action(%s, %s, note %q, dry_run %v) = %q, want %q", c.decision, c.update, c.checkNote, c.ev.cfg.DryRun, got, c.want)
		}
	}
}

func TestDecodeRecorded(t *testing.T) {
	got := decodeRecorded([]byte(`{"current":{"tag":"latest","version":" 1.27.1 "},` +
		`"candidate":{"tag":"latest","version":"1.27.2"},"check_note":"dial tcp: connection refused"}`))
	if got != (recorded{currentVersion: "1.27.1", candidateVersion: "1.27.2", checkNote: "dial tcp: connection refused"}) {
		t.Errorf("decodeRecorded = %+v", got)
	}
	if got := decodeRecorded([]byte(`{"current":{"tag":"latest"}}`)); got != (recorded{}) {
		t.Errorf("a status without them = %+v", got)
	}
	if got := decodeRecorded([]byte(`not json`)); got != (recorded{}) {
		t.Errorf("garbage = %+v", got)
	}
	s := updater.ContainerStatus{Host: "local", Container: "web", Current: updater.ImageVersion{Tag: "latest"}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if recordedOf(s) != decodeRecorded(b) {
		t.Errorf("recordedOf(%+v) = %+v", s, recordedOf(s))
	}
}

func TestExplainGates(t *testing.T) {
	windows, err := window.Compile("UTC", []window.Spec{{Days: []string{"Mon"}, Times: []string{"02:00-03:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	ev := &evaluation{cfg: &config.Config{Timezone: "UTC"}, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	r := resolved{Resolved: policy.Resolved{PolicyID: "weekly", Mode: config.ModeTagFollow, Windows: windows}, source: "label", label: "weekly"}
	verdicts := func(s updater.ContainerStatus, e planEntry) (order []string, v map[string]gate) {
		v = map[string]gate{}
		for _, g := range ev.gates(s, r, e) {
			order = append(order, g.Gate)
			v[g.Gate] = g
		}
		return order, v
	}

	s := updater.ContainerStatus{Decision: "gated-window", Update: updater.UpdateAvailable, Reason: `outside policy "weekly" window`}
	e := planEntry{Current: planImage{Tag: "3"}, Candidate: &planImage{Tag: "3", Digest: "sha256:f2f2f2f2f2f2f2f2"}, Reason: s.Reason}
	order, v := verdicts(s, e)
	if !slices.Equal(order, []string{"filters", "policy", "container", "mode", "digest pin", "registry", "window",
		"soak", "monitor-only", "dry-run", "approval", "update"}) {
		t.Errorf("gates %q", order)
	}
	if g := v[gateRegistry]; g.Verdict != verdictPass || !strings.Contains(g.Detail, "newer image") {
		t.Errorf("registry = %+v", g)
	}
	if g := v[gateWindow]; g.Verdict != verdictStop || !strings.Contains(g.Change, `change the windows of policy "weekly"`) {
		t.Errorf("window = %+v", g)
	}
	if v[gateSoak].Verdict != verdictNotReached || v[gatePin].Verdict != verdictPass {
		t.Errorf("soak %+v, digest pin %+v", v[gateSoak], v[gatePin])
	}
	s.Update, e.Candidate = updater.UpdateUnknown, nil
	if _, v := verdicts(s, e); v[gateRegistry].Verdict != verdictNotReached || v[gateWindow].Verdict != verdictStop {
		t.Errorf("unknown outside the window: registry %+v, window %+v", v[gateRegistry], v[gateWindow])
	}

	s = updater.ContainerStatus{Decision: "gated-policy", Update: updater.UpdateAvailable, Reason: "held"}
	e = planEntry{Candidate: &planImage{Digest: "sha256:aa"}, Reason: s.Reason}
	r.Windows = nil
	order, v = verdicts(s, e)
	if i := slices.Index(order, gateSelfUpdate); i < 0 || order[i-1] != gateWindow || order[i+1] != gateSoak || v[gateSelfUpdate].Verdict != verdictStop {
		t.Errorf("self-update gates %q (%+v)", order, v[gateSelfUpdate])
	}

	s = updater.ContainerStatus{Decision: "check-incomplete", Update: updater.UpdateUnknown, Reason: "not checked"}
	e = planEntry{Reason: s.Reason, checkNote: "dial tcp: connection refused"}
	if _, v := verdicts(s, e); v[gateRegistry].Verdict != verdictStop || !strings.Contains(v[gateRegistry].Change, "ask the registry directly") {
		t.Errorf("registry not asked = %+v", v[gateRegistry])
	}
	e.checkNote = ""
	if _, v := verdicts(s, e); !strings.Contains(v[gateRegistry].Change, "a tag the registry lists versions of") {
		t.Errorf("no comparable tags = %+v", v[gateRegistry])
	}
}

func TestImageText(t *testing.T) {
	cur := planImage{Tag: "latest", Version: "1.27.1", Digest: "sha256:0123456789abcdef0123"}
	cand := planImage{Tag: "latest", Version: "1.27.2", Digest: "sha256:fedcba9876543210fedc"}
	e := planEntry{Current: cur, Candidate: &cand}
	if got := changeText(e); got != "latest (1.27.1) 0123456789ab -> (1.27.2) fedcba987654" {
		t.Errorf("changeText = %q", got)
	}
	e = planEntry{Current: planImage{Tag: "16.4", Digest: "sha256:aa"}, Candidate: &planImage{Tag: "16.6", Version: "16.6", Digest: "sha256:bb"}}
	if got := changeText(e); got != "16.4 aa -> 16.6 bb" {
		t.Errorf("changeText with a new tag = %q", got)
	}
	if got := imageText(planImage{}, ""); got != "-" {
		t.Errorf("empty image = %q", got)
	}
}

func TestPolicyLabelText(t *testing.T) {
	cases := []struct {
		e    planEntry
		want string
	}{
		{planEntry{Policy: "default", PolicySource: "default"}, "default"},
		{planEntry{Policy: "database", PolicySource: "label"}, "database (label)"},
		{planEntry{Policy: "database", PolicySource: "label", Overrides: []string{policy.LabelMode}}, "database (label, overridden)"},
		{planEntry{Policy: "default", PolicySource: "default", Overrides: []string{policy.LabelMinImageAge}}, "default (overridden)"},
		{planEntry{PolicySource: "default"}, "-"},
	}
	for _, c := range cases {
		if got := policyLabelText(c.e); got != c.want {
			t.Errorf("policyLabelText(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}
