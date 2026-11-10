package probe

import (
	"context"
	"runtime/debug"
	"strconv"
	"strings"

	clientversion "k8s.io/client-go/pkg/version"
)

type ServerVersionInfo struct {
	GitVersion   string `json:"gitVersion,omitempty"`
	Major        string `json:"major,omitempty"`
	Minor        string `json:"minor,omitempty"`
	GitCommit    string `json:"gitCommit,omitempty"`
	GitTreeState string `json:"gitTreeState,omitempty"`
	BuildDate    string `json:"buildDate,omitempty"`
	GoVersion    string `json:"goVersion,omitempty"`
	Platform     string `json:"platform,omitempty"`
}

// VersionInfo carries the server version and the client/server skew. Official
// skew policy allows one minor version either way.
type VersionInfo struct {
	Server           ServerVersionInfo `json:"server"`
	Client           string            `json:"client,omitempty"`
	MinorDelta       int               `json:"minorDelta"`
	WithinSkewWindow bool              `json:"withinSkewWindow"`
}

func (p *prober) probeVersion(ctx context.Context) (VersionInfo, []ProbeError) {
	info, err := p.in.Client.Discovery().ServerVersion()
	if err != nil {
		p.versionErr = err
		return VersionInfo{}, []ProbeError{asProbeError("version", "serverversion", err)}
	}
	v := VersionInfo{
		Server: ServerVersionInfo{
			GitVersion:   info.GitVersion,
			Major:        info.Major,
			Minor:        info.Minor,
			GitCommit:    info.GitCommit,
			GitTreeState: info.GitTreeState,
			BuildDate:    info.BuildDate,
			GoVersion:    info.GoVersion,
			Platform:     info.Platform,
		},
		Client: clientGoVersion(),
	}
	serverMinor, okServer := parseMinor(info.Minor, info.GitVersion)
	clientMinor, okClient := parseMinorVersion(v.Client)
	switch {
	case okServer && okClient:
		v.MinorDelta = serverMinor - clientMinor
		if v.MinorDelta < 0 {
			v.MinorDelta = -v.MinorDelta
		}
		v.WithinSkewWindow = v.MinorDelta <= 1
	default:
		// Cannot determine skew: report no delta and do not alarm.
		v.WithinSkewWindow = true
	}
	return v, nil
}

// clientGoVersion reports the client-go version actually linked into the
// binary. The package-level version.Info is a placeholder for ad-hoc builds,
// so fall back to the module version from the build info.
func clientGoVersion() string {
	if v := clientversion.Get().GitVersion; v != "" && strings.Count(v, ".") >= 2 && !strings.Contains(v, "$Format") {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, d := range info.Deps {
			if d.Path == "k8s.io/client-go" && d.Version != "" {
				return d.Version
			}
		}
	}
	return "unknown"
}

func parseMinor(minorField, gitVersion string) (int, bool) {
	if n, ok := parseMinorString(minorField); ok {
		return n, true
	}
	return parseMinorString(gitVersion)
}

func parseMinorVersion(v string) (int, bool) {
	return parseMinorString(v)
}

// parseMinorString extracts the minor number from strings like "35", "35+",
// "v1.35.5+k3s1", or the client-go module version "v0.37.2".
func parseMinorString(s string) (int, bool) {
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimSuffix(s, "+")
	parts := strings.SplitN(s, ".", 3)
	var candidate string
	switch {
	case len(parts) >= 2 && (parts[0] == "1" || parts[0] == "0") && isDigits(parts[1]):
		// "1.35.5+k3s1" → 35, "0.37.2" → 37
		candidate = parts[1]
	case isDigits(parts[0]):
		// "35" or "35+" → 35
		candidate = parts[0]
	default:
		return 0, false
	}
	n, err := strconv.Atoi(candidate)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
