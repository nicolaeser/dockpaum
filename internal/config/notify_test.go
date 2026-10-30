package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNotifierOnce(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte(`notify:
  - type: webhook
    url: https://hooks.example.com/a
  - type: webhook
    url: https://hooks.example.com/b
    once: false
  - type: webhook
    url: https://hooks.example.com/c
    once: true
`), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify[0].Once != nil || !c.Notify[0].ReportsOnce() {
		t.Errorf("unset once = %v, want nil (reports once)", c.Notify[0].Once)
	}
	if c.Notify[1].Once == nil || *c.Notify[1].Once || c.Notify[1].ReportsOnce() {
		t.Errorf("once: false = %v, want every run", c.Notify[1].Once)
	}
	if !c.Notify[2].ReportsOnce() {
		t.Error("once: true must report once")
	}
	red := c.Redacted()
	for i, want := range []bool{true, false, true} {
		if got := red.Notify[i].Once; got == nil || *got != want {
			t.Errorf("--print-config notify[%d].once = %v, want %v", i, got, want)
		}
	}
	if c.Notify[0].Once != nil {
		t.Error("Redacted materialized once in the loaded config")
	}
}

func TestSMTPPortDefault(t *testing.T) {
	isolateEnv(t)
	c, err := decode([]byte(`notify:
  - type: email
    to: [ops@example.com]
    smtp: {host: mail.example.com, starttls: true}
  - type: email
    to: [ops@example.com]
    smtp: {host: mail.example.com, port: 465}
`), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Notify[0].SMTP.Port; got != DefaultSMTPPort {
		t.Errorf("smtp.port = %d, want %d", got, DefaultSMTPPort)
	}
	if got := c.Notify[1].SMTP.Port; got != 465 {
		t.Errorf("an explicit port changed to %d", got)
	}
	out, err := yaml.Marshal(c.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "port: 587") {
		t.Errorf("--print-config does not show the effective port:\n%s", out)
	}
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "starttls") {
		t.Errorf("warnings = %s", w)
	}
	for _, port := range []string{"-1", "65536"} {
		err := decodeErr(t, "notify:\n  - type: email\n    to: [ops@example.com]\n    smtp:\n      host: mail.example.com\n      port: "+port+"\n")
		if want := "dockpaum.yml:6: notify[0] (email).smtp.port: " + port + " is not a port number (want 1 to 65535; default 587)"; !strings.Contains(err.Error(), want) {
			t.Errorf("port %s: err = %v, want %q", port, err, want)
		}
	}
}
