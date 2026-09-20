package storagehelper

import (
	"fmt"
	"strings"
)

func embeddedComponent() ([]byte, string, error) {
	binary, checksum, err := readEmbeddedComponent()
	if err != nil {
		return nil, "", err
	}
	expected := strings.TrimSpace(string(checksum))
	if len(expected) != 64 || strings.ToLower(expected) != expected {
		return nil, "", fmt.Errorf("embedded storage helper checksum is malformed")
	}
	for _, character := range expected {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return nil, "", fmt.Errorf("embedded storage helper checksum is malformed")
		}
	}
	return binary, expected, nil
}
