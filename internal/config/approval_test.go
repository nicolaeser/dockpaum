package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalSigningConfig(t *testing.T) {
	isolateEnv(t)
	secret := filepath.Join(t.TempDir(), "approval-secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("k", MinApprovalSecretLen)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := func(signing string) string {
		return "update:\n  approval:\n    url: https://approver.example.com/approve\n    recheck_before_stop: true\n" + signing
	}

	c, err := decode([]byte(doc("    signing:\n      secret_file: \" "+secret+" \"\n")), "dockpaum.yml")
	if err != nil {
		t.Fatal(err)
	}
	a := c.Update.Approval
	if !a.RecheckBeforeStop || a.Signing.KeyID != DefaultApprovalKeyID || a.Signing.SecretFile != secret {
		t.Fatalf("approval %+v signing %+v", a, a.Signing)
	}
	if got, err := a.Signing.Secret(); err != nil || string(got) != strings.Repeat("k", MinApprovalSecretLen) {
		t.Fatalf("secret %q, %v: surrounding whitespace is trimmed", got, err)
	}
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "signing") {
		t.Fatalf("unexpected warning: %s", w)
	}
	red := c.Redacted()
	if red.Update.Approval.Signing.SecretFile != secret || red.Update.Approval.Signing == a.Signing {
		t.Fatal("print-config shows the secret_file path, from a copy")
	}

	bad := []struct{ name, signing, want string }{
		{"no secret_file", "    signing:\n      key_id: k1\n", "secret_file: required"},
		{"bad key_id", "    signing:\n      key_id: \"a b\"\n      secret_file: " + secret + "\n", "signing.key_id"},
		{"long key_id", "    signing:\n      key_id: " + strings.Repeat("k", 65) + "\n      secret_file: " + secret + "\n", "signing.key_id"},
		{"signature header set by hand", "    headers:\n      x-dockpaum-signature: v1=00\n    signing:\n      secret_file: " + secret + "\n", "X-Dockpaum-Signature is set by signing"},
		{"unknown key", "    signing:\n      secret: inline\n      secret_file: " + secret + "\n", "secret"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decode([]byte(doc(tc.signing)), "dockpaum.yml")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestApprovalSigningSecretWarnings(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("too-short-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"missing": filepath.Join(dir, "absent"), "short": short} {
		t.Run(name, func(t *testing.T) {
			w := warningsOf(t, "update:\n  approval:\n    url: https://approver.example.com/approve\n    signing:\n      secret_file: "+file+"\n")
			if !strings.Contains(w, "update.approval.signing.secret_file") || !strings.Contains(w, "every update is denied") {
				t.Fatalf("warnings: %s", w)
			}
			if strings.Contains(w, "too-short-secret") {
				t.Fatal("a warning must never show the secret")
			}
		})
	}
}

func TestApprovalApproveOn(t *testing.T) {
	isolateEnv(t)
	doc := func(extra string) string {
		return "update:\n  approval:\n    url: https://status.example.com/freeze\n" + extra
	}
	for _, tc := range []struct{ yaml, want string }{
		{"", ""},
		{"    approve_on: success\n", ApproveOnSuccess},
		{"    approve_on: \" Failure \"\n", ApproveOnFailure},
	} {
		c, err := decode([]byte(doc(tc.yaml)), "dockpaum.yml")
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if a := c.Update.Approval; a.ApproveOn != tc.want || a.ApprovesOnFailure() != (tc.want == ApproveOnFailure) {
			t.Fatalf("%q: approve_on %q", tc.yaml, a.ApproveOn)
		}
	}
	_, err := decode([]byte(doc("    approve_on: failed\n")), "dockpaum.yml")
	if err == nil || !strings.Contains(err.Error(), `update.approval.approve_on: unknown approve_on "failed"`) || !strings.Contains(err.Error(), "(want success or failure)") {
		t.Fatalf("err = %v", err)
	}

	w := warningsOf(t, doc("    approve_on: failure\n    fail_open: true\n"))
	if !strings.Contains(w, "update.approval.fail_open has no effect with approve_on: failure") || strings.Contains(w, "go through while the approver is unreachable") {
		t.Fatalf("warnings: %s", w)
	}
	if w := warningsOf(t, doc("    approve_on: failure\n")); strings.Contains(w, "fail_open") {
		t.Fatalf("warnings without fail_open: %s", w)
	}
	if w := warningsOf(t, doc("    fail_open: true\n")); !strings.Contains(w, "update.approval.fail_open: updates go through while the approver is unreachable") {
		t.Fatalf("warnings with approve_on: success: %s", w)
	}
}
