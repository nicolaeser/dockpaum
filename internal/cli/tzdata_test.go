package cli

import (
	"go/build"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestTimezoneDataEmbedded(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pkg.Imports, "time/tzdata") {
		t.Fatal("cmd/dockpaum must import time/tzdata: the runtime image has no zoneinfo, so every non-UTC timezone would fail")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTimezoneLookupHelper$", "-test.count=1") // #nosec G204 G702 -- re-runs this test binary
	cmd.Env = append(os.Environ(), "DOCKPAUM_TZ_HELPER=1", "ZONEINFO=/nonexistent/zoneinfo.zip")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Europe/Berlin did not load with ZONEINFO pointing nowhere: %v\n%s", err, out)
	}
}

func TestTimezoneLookupHelper(t *testing.T) {
	if os.Getenv("DOCKPAUM_TZ_HELPER") != "1" {
		t.Skip("subprocess helper of TestTimezoneDataEmbedded")
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at     time.Time
		offset int
	}{
		{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), 3600},
		{time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC), 7200},
	} {
		if _, off := c.at.In(loc).Zone(); off != c.offset {
			t.Errorf("Europe/Berlin offset at %s = %d, want %d", c.at, off, c.offset)
		}
	}
}
