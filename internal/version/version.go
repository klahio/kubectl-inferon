// Package version holds build information. Release builds set it through -ldflags (see .goreleaser.yaml).
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Name is the plugin's name: the binary is "kubectl-" + Name and users type "kubectl " + Name. It is the only place
// the name is spelled out, so a rename stays cheap (docs/mvp.md, decision 5).
const Name = "inferon"

// Command is how users invoke the plugin, for messages that tell them what to run next.
const Command = "kubectl " + Name

// Set by the linker, for example -X github.com/klahio/kubectl-inferon/internal/version.Version=v0.1.0.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String is the one-line version shown by `kubectl inferon version` and `--version`.
func String() string {
	commit, date := Commit, Date
	// A `go install` build has no linker flags, but Go records the commit it was built from.
	if commit == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					commit = s.Value
				case "vcs.time":
					date = s.Value
				}
			}
			if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
				Version = info.Main.Version
			}
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	s := Version
	if commit != "" {
		s += " (commit " + commit
		if date != "" {
			s += ", built " + date
		}
		s += ")"
	}
	return fmt.Sprintf("%s %s/%s", s, runtime.GOOS, runtime.GOARCH)
}
