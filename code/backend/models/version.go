package models

import (
	"fmt"
	"strconv"
	"strings"
)

const ReplicaroVersion = "1.0.2"

type SemanticVersion struct {
	Major uint64
	Minor uint64
	Patch uint64
}

func ParseSemanticVersion(value string) (SemanticVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return SemanticVersion{}, fmt.Errorf("version must use MAJOR.MINOR.PATCH")
	}
	values := make([]uint64, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return SemanticVersion{}, fmt.Errorf("version must use canonical decimal components")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return SemanticVersion{}, fmt.Errorf("version must contain decimal components only")
			}
		}
		parsed, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return SemanticVersion{}, fmt.Errorf("version component exceeds uint64: %w", err)
		}
		values[index] = parsed
	}
	return SemanticVersion{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func CompareSemanticVersions(left, right SemanticVersion) int {
	leftParts := [...]uint64{left.Major, left.Minor, left.Patch}
	rightParts := [...]uint64{right.Major, right.Minor, right.Patch}
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}
