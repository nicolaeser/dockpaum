package buildinfo

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	if Version != DefaultVersion || Commit != DefaultCommit || Date != DefaultDate {
		t.Fatalf("a test build carries ldflags: %q %q %q", Version, Commit, Date)
	}
	if DefaultVersion != "dev" || DefaultCommit != "unknown" || DefaultDate != "unknown" {
		t.Fatalf("defaults %q %q %q, want dev/unknown/unknown", DefaultVersion, DefaultCommit, DefaultDate)
	}
}

type marker struct{}

func TestBuildsStampThisPackage(t *testing.T) {
	pkg := reflect.TypeOf(marker{}).PkgPath()
	for _, file := range []string{"../../Makefile", "../../Dockerfile"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"Version", "Commit", "Date"} {
			if want := "-X " + pkg + "." + v + "="; !strings.Contains(string(raw), want) {
				t.Errorf("%s does not set %s", file, want)
			}
		}
	}
}
