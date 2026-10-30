package cli

import (
	"encoding/json"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/nicolaeser/dockpaum/internal/buildinfo"
	"github.com/nicolaeser/dockpaum/internal/server"
)

func buildVersion() server.Version {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return resolveVersion(buildinfo.Version, buildinfo.Commit, buildinfo.Date, bi)
}

func resolveVersion(v, c, d string, bi *debug.BuildInfo) server.Version {
	out := server.Version{
		Version: v, Commit: c, Date: d,
		Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi == nil {
		return out
	}
	if bi.GoVersion != "" {
		out.Go = bi.GoVersion
	}
	if out.Version == buildinfo.DefaultVersion && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		out.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && out.Commit == buildinfo.DefaultCommit && s.Value != "":
			out.Commit = s.Value
			if len(out.Commit) > 7 {
				out.Commit = out.Commit[:7]
			}
		case s.Key == "vcs.time" && out.Date == buildinfo.DefaultDate && s.Value != "":
			out.Date = s.Value
		}
	}
	return out
}

func runVersion(inv *invocation, stdout io.Writer) int {
	v := buildVersion()
	if inv.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return exitFailure
		}
		return exitOK
	}
	printf(stdout, "dockpaum %s (commit %s, built %s, %s, %s)\n", v.Version, v.Commit, v.Date, v.Go, v.Platform)
	return exitOK
}
