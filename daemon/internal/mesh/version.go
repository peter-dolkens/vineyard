package mesh

import (
	"regexp"
	"strconv"
)

// Version comparison for upgrades, matching src/core/version.ts in the extension: only the numeric
// core (major.minor.patch, optional leading "v") counts. Anything after it ("-dev", "-3-gabc123",
// "+build") is ignored, so two dev builds of one release never fight over which is newer, and an
// unparseable version ("dev", "test", "") never triggers an upgrade in either direction.

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

type parsedVersion struct{ major, minor, patch int }

func parseVersion(v string) (parsedVersion, bool) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return parsedVersion{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return parsedVersion{a, b, c}, true
}

// isNewer is true only when both versions parse and candidate's numeric core is strictly greater.
func isNewer(candidate, current string) bool {
	a, ok1 := parseVersion(candidate)
	b, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	if a.major != b.major {
		return a.major > b.major
	}
	if a.minor != b.minor {
		return a.minor > b.minor
	}
	return a.patch > b.patch
}
