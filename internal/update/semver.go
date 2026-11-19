package update

import (
	"strconv"
	"strings"
)

// semver is a parsed semantic version (https://semver.org); build metadata is
// ignored, as the precedence rules say.
type semver struct {
	major, minor, patch uint64
	pre                 []string // dot-separated pre-release identifiers
}

// parseSemver accepts "1.2.3", "v1.2.3", "1.2.3-rc.1" and "1.2.3+build".
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return semver{}, false
		}
		nums[i] = n
	}
	v := semver{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		if pre == "" {
			return semver{}, false
		}
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" {
				return semver{}, false
			}
		}
	}
	return v, true
}

func (v semver) prerelease() bool { return len(v.pre) > 0 }

// compare returns -1, 0 or 1 following the semver precedence rules.
func (v semver) compare(o semver) int {
	for _, p := range [][2]uint64{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case !v.prerelease() && !o.prerelease():
		return 0
	case !v.prerelease():
		return 1 // a release is newer than its pre-releases
	case !o.prerelease():
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(o.pre); i++ {
		if c := comparePreID(v.pre[i], o.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.pre) < len(o.pre):
		return -1
	case len(v.pre) > len(o.pre):
		return 1
	}
	return 0
}

func comparePreID(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// Newer reports whether latest is a strictly newer version than current. It is
// false when either side is not a valid semantic version.
func Newer(latest, current string) bool {
	l, ok := parseSemver(latest)
	if !ok {
		return false
	}
	c, ok := parseSemver(current)
	if !ok {
		return false
	}
	return l.compare(c) > 0
}
