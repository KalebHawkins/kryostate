package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestLinkerValuesWin(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.1"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "ffffffffffffffffffff"},
		{Key: "vcs.modified", Value: "true"},
	}}
	in := resolve("v1.2.3", "abc1234", "2026-11-12T00:00:00Z", bi, true)
	if in.Version != "v1.2.3" || in.Commit != "abc1234" || in.Date != "2026-11-12T00:00:00Z" || in.Modified {
		t.Errorf("got %+v", in)
	}
}

func TestBuildInfoFillsTheGaps(t *testing.T) {
	bi := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123"},
		{Key: "vcs.time", Value: "2026-03-01T15:04:05Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	in := resolve("", "", "", bi, true)
	if in.Version != "dev" || in.Commit != "0123456789ab" || in.Date != "2026-03-01T15:04:05Z" || !in.Modified {
		t.Errorf("got %+v", in)
	}
	if !strings.Contains(in.String(), "(modified)") {
		t.Errorf("String() hides the modified build: %s", in.String())
	}
}

func TestGoInstallReportsTheModuleVersion(t *testing.T) {
	in := resolve("", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}}, true)
	if in.Version != "v0.4.0" || in.Commit != "unknown" || in.Date != "unknown" {
		t.Errorf("got %+v", in)
	}
}
