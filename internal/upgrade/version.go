package upgrade

import (
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Version is a parsed semantic version. Release manifests use plain semantic
// versions ("1.2.3", optional "v" prefix and prerelease suffix). Development
// builds report "dev" and are never ordered against a release.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

// ParseVersion parses a release version. Build metadata (+...) is accepted and
// ignored for ordering; prerelease identifiers are compared lexically.
func ParseVersion(text string) (Version, error) {
	original := text
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "v"))
	if text == "" {
		return Version{}, versionFault("release version is empty")
	}
	if index := strings.IndexByte(text, '+'); index >= 0 {
		text = text[:index]
	}
	prerelease := ""
	if index := strings.IndexByte(text, '-'); index >= 0 {
		prerelease = text[index+1:]
		text = text[:index]
		if prerelease == "" {
			return Version{}, versionFault("release version has an empty prerelease")
		}
	}
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return Version{}, versionFault("release version must be MAJOR.MINOR.PATCH: " + original)
	}
	numbers := make([]int, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, versionFault("release version has an invalid numeric component: " + original)
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return Version{}, versionFault("release version has an invalid numeric component: " + original)
		}
		numbers[index] = value
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2], Prerelease: prerelease}, nil
}

func (v Version) String() string {
	text := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Prerelease != "" {
		text += "-" + v.Prerelease
	}
	return text
}

// Compare returns -1, 0, or 1. Prerelease versions sort before their release.
func (v Version) Compare(other Version) int {
	for _, pair := range [][2]int{{v.Major, other.Major}, {v.Minor, other.Minor}, {v.Patch, other.Patch}} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

func comparePrerelease(left, right string) int {
	if left == right {
		return 0
	}
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for index := 0; index < len(leftParts) || index < len(rightParts); index++ {
		if index >= len(leftParts) {
			return -1
		}
		if index >= len(rightParts) {
			return 1
		}
		leftNumber, leftNumeric := numericIdentifier(leftParts[index])
		rightNumber, rightNumeric := numericIdentifier(rightParts[index])
		switch {
		case leftNumeric && rightNumeric:
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftParts[index] < rightParts[index]:
			return -1
		case leftParts[index] > rightParts[index]:
			return 1
		}
	}
	return 0
}

func numericIdentifier(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}

func versionFault(message string) error {
	return contract.NewFault(contract.ValidationFailed, message, "Use a semantic version such as 1.2.3.")
}

// contractCompatible reports whether a manifest contract matches the running
// binary. The envelope contract is major-versioned; a binary may read the exact
// contract it implements, never an unknown one.
func contractCompatible(manifest, running string) bool {
	return strings.TrimSpace(manifest) != "" && manifest == running
}
