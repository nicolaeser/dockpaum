package config

import (
	"strings"
	"testing"
)

func TestWarnings(t *testing.T) {
	isolateEnv(t)
	cfg, err := decode([]byte(`
hosts: [{ name: nas, endpoint: "tcp://nas:2375" }]
update:
  rollback_on_failure: false
  approval: { url: "http://approve.example.com", fail_open: true }
notify:
  - { type: email, to: [a@b.c], smtp: { host: mail.example.com } }
hooks: { label_hooks: true, pre_update: ["echo ${DOCKPAUM_TARGET}"] }
`), "warn")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{"tcp:// without TLS", "rollback_on_failure is false", "fail_open", "approval.url: plain http://", "starttls is off", "label_hooks"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings miss %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pre_update") {
		t.Error("${DOCKPAUM_*} in hooks is legitimate and must not warn")
	}
	clean, err := decode([]byte("update:\n  policies:\n    - id: default\n      health_timeout: 2m\n"), "clean")
	if err != nil {
		t.Fatal(err)
	}
	if w := clean.Warnings(); len(w) != 0 {
		t.Errorf("a clean config warned: %v", w)
	}
}

func TestDeferredRefsInSecretsAreErrors(t *testing.T) {
	isolateEnv(t)
	for _, yaml := range []string{
		"http:\n  token: \"${DOCKPAUM_TOKEN}\"\n",
		"registry:\n  - server: ghcr.io\n    username: u\n    password: \"${DOCKPAUM_PASS}\"\n",
		"notify:\n  - type: slack\n    url: \"${DOCKPAUM_SLACK}\"\n",
		"update:\n  approval:\n    url: https://a\n    headers: { Authorization: \"Bearer ${DOCKPAUM_T}\" }\n",
	} {
		if _, err := decode([]byte(yaml), "deferred"); err == nil || !strings.Contains(err.Error(), "DOCKPAUM_") {
			t.Errorf("decode(%q) = %v, want a ${DOCKPAUM_*} error", yaml, err)
		}
	}
	if _, err := decode([]byte("hooks:\n  pre_update: [\"echo ${DOCKPAUM_TARGET}\"]\njobs:\n  - name: j\n    cron: \"@daily\"\n    commands: [\"echo $DOCKPAUM_JOB\"]\n"), "ok"); err != nil {
		t.Errorf("${DOCKPAUM_*} in hooks and jobs must stay valid: %v", err)
	}
}

func TestParallelWithComposeGroupWarns(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte("update:\n  strategy: parallel\n  compose_group: true\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, "parallel has no effect with update.compose_group: true") {
		t.Errorf("warnings = %q", w)
	}
	c, err = decode([]byte("update:\n  strategy: parallel\n"), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "parallel") {
			t.Errorf("parallel without compose_group warned: %s", w)
		}
	}
}
