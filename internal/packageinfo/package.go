package packageinfo

import (
	"regexp"
	"strings"
)

func BasePort(origin string) string {
	if end := strings.IndexAny(origin, "@~"); end >= 0 {
		return origin[:end]
	}
	return origin
}

func ValidIssuePackage(pkg string) bool {
	return len(pkg) <= 255 && JobPackage.MatchString(pkg)
}

var JobPackage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+,~-]*$`)

var BuildOrigin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*/[A-Za-z0-9][A-Za-z0-9_.+-]*(@[A-Za-z0-9][A-Za-z0-9_.+-]*)?(~[A-Za-z0-9][A-Za-z0-9_.+-]*)?$`)

type Package struct {
	Path, Name, Origin, BuildOrigin, Version, Sum, ABI string
	Size                                               int64
	Local                                              bool
	Live                                               bool   `json:"-"`
	OverlayDigest                                      string `json:"-"`
}

func PackageBuildOrigin(origin, flavor, subpackage string) string {
	if flavor != "" {
		origin += "@" + flavor
	}
	if subpackage != "" {
		origin += "~" + subpackage
	}
	return origin
}

func PackageABI(abi, arch string) string {
	if abi == "" {
		abi = arch
	}
	abi = strings.ToLower(abi)
	abi = strings.ReplaceAll(abi, ":x86:64", ":amd64")
	return strings.ReplaceAll(abi, ":x86:32", ":i386")
}

func CompatibleABI(a, b string) bool {
	if a == b {
		return true
	}
	if prefix, ok := strings.CutSuffix(a, ":*"); ok {
		return strings.HasPrefix(b, prefix+":")
	}
	if prefix, ok := strings.CutSuffix(b, ":*"); ok {
		return strings.HasPrefix(a, prefix+":")
	}
	return false
}

// Poudriere stores the full pkgname, including the optional revision and epoch.
// FreeBSD package names may contain hyphens; their versions cannot.
func SplitBuildPackage(pkg string) (name, version string) {
	if !ValidIssuePackage(pkg) {
		return "", ""
	}
	return SplitNameVersion(pkg)
}

// SplitNameVersion separates a pkgname after the caller has checked the
// syntax and size permitted by its input format.
func SplitNameVersion(pkg string) (name, version string) {
	i := strings.LastIndexByte(pkg, '-')
	if i < 1 || i == len(pkg)-1 {
		return "", ""
	}
	return pkg[:i], pkg[i+1:]
}
