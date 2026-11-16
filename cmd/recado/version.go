package main

import "runtime/debug"

// version is set by the release build (-ldflags "-X main.version=v1.2.3").
var version string

// buildVersion is the release version, else the module version "go install"
// recorded, else "dev" for a local build.
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
