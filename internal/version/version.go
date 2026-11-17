// Package version reports the build version.
package version

import (
	"runtime/debug"
	"strings"
)

// version is set at link time:
//
//	-ldflags "-X github.com/henry40408/domus/internal/version.version=$GIT_VERSION"
//
// Docker builds have no .git directory, so they pass it in.
var version string

// Version returns the injected version, else the VCS-stamped module version,
// else "unknown". An injected "dev" counts as unset.
func Version() string {
	if version != "" && version != "dev" {
		return version
	}
	return buildInfoVersion()
}

func buildInfoVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return moduleVersion(info.Main.Version)
}

// moduleVersion maps the versions Go stamps for a build outside a tagged
// module ("" and "(devel)") to "unknown".
func moduleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return "unknown"
	}
	return v
}

// Cacheable reports whether v names a fixed build. Static assets are served
// as immutable only for such a version: a changing build must not be cached
// for a year under one URL.
func Cacheable(v string) bool {
	switch v {
	case "", "dev", "unknown":
		return false
	}
	return !strings.HasSuffix(v, "-dirty") && !strings.Contains(v, "+dirty")
}

// HAVersion is the Home Assistant version domus claims to be. The Companion
// apps gate features on it, so it must not be raised without checking what
// they then expect from the server.
const HAVersion = "2025.12.0"
