package database

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math/bits"
	"strings"

	"github.com/local/replicaro/integrations"
)

const secretCodecPrefix = "obscured:v1:"

var (
	secretCodecCRC = crc32.MakeTable(crc32.Castagnoli)
	// secretCodecMask must remain the exact lowercase phrase "replicaro".
	secretCodecMask = [...]byte{
		0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x72, 0x6f,
	}
)

// encodeSecret deterministically obscures bytes for casual SQLite viewing.
// This is reversible obfuscation, not encryption, authentication, or security.
func encodeSecret(plaintext []byte) string {
	if len(plaintext) == 0 {
		return ""
	}
	frame := make([]byte, 12+len(plaintext))
	binary.BigEndian.PutUint64(frame[:8], uint64(len(plaintext)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.Checksum(plaintext, secretCodecCRC))
	for index, value := range plaintext {
		rotated := bits.RotateLeft8(value, (index%7)+1)
		frame[12+index] = rotated ^ secretCodecMask[index%len(secretCodecMask)]
	}
	return secretCodecPrefix + base64.RawURLEncoding.EncodeToString(frame)
}

func decodeSecret(stored string) ([]byte, error) {
	if stored == "" {
		return nil, nil
	}
	if !strings.HasPrefix(stored, secretCodecPrefix) {
		if strings.HasPrefix(stored, "obscured:") {
			return nil, fmt.Errorf("stored secret uses an unsupported format")
		}
		return nil, fmt.Errorf("stored secret is not obscured")
	}
	payload := strings.TrimPrefix(stored, secretCodecPrefix)
	frame, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil || base64.RawURLEncoding.EncodeToString(frame) != payload {
		return nil, fmt.Errorf("stored secret payload is malformed")
	}
	if len(frame) < 12 {
		return nil, fmt.Errorf("stored secret payload is truncated")
	}
	length := binary.BigEndian.Uint64(frame[:8])
	if length > uint64(len(frame)-12) || length != uint64(len(frame)-12) {
		return nil, fmt.Errorf("stored secret length is inconsistent")
	}
	plaintext := make([]byte, int(length))
	for index, value := range frame[12:] {
		unmasked := value ^ secretCodecMask[index%len(secretCodecMask)]
		plaintext[index] = bits.RotateLeft8(unmasked, -((index % 7) + 1))
	}
	if crc32.Checksum(plaintext, secretCodecCRC) != binary.BigEndian.Uint32(frame[8:12]) {
		return nil, fmt.Errorf("stored secret checksum is inconsistent")
	}
	return plaintext, nil
}

func catalogOptions(connector string) (map[string]integrations.Option, error) {
	integration, ok := integrations.Find(connector)
	if !ok {
		return nil, fmt.Errorf("repository connector is not in the integration catalog")
	}
	options := make(map[string]integrations.Option, len(integration.Options))
	for _, option := range integration.Options {
		if option.Key == "" {
			return nil, fmt.Errorf("integration catalog contains an invalid option")
		}
		options[option.Key] = option
	}
	return options, nil
}

func transformClassifiedOptions(connector string, options map[string]string, encode bool) (map[string]string, error) {
	catalog, err := catalogOptions(connector)
	if err != nil {
		return nil, err
	}
	transformed := make(map[string]string, len(options))
	for key, value := range options {
		option, ok := catalog[key]
		if !ok {
			return nil, fmt.Errorf("repository connector option is not in the integration catalog")
		}
		if !option.Credential && !option.Secret || value == "" {
			transformed[key] = value
			continue
		}
		if encode {
			transformed[key] = encodeSecret([]byte(value))
			continue
		}
		decoded, err := decodeSecret(value)
		if err != nil {
			return nil, err
		}
		transformed[key] = string(decoded)
	}
	return transformed, nil
}

func encodeRepositorySecrets(connector, passphrase string, options map[string]string) (string, string, error) {
	encodedOptions, err := transformClassifiedOptions(connector, options, true)
	if err != nil {
		return "", "", err
	}
	optionsJSON, err := json.Marshal(encodedOptions)
	if err != nil {
		return "", "", fmt.Errorf("encode connector options")
	}
	return encodeSecret([]byte(passphrase)), string(optionsJSON), nil
}

func decodeRepositorySecrets(connector, passphrase, optionsJSON string) (string, map[string]string, error) {
	options := map[string]string{}
	if optionsJSON != "" {
		if err := json.Unmarshal([]byte(optionsJSON), &options); err != nil {
			return "", nil, fmt.Errorf("stored connector options are malformed")
		}
	}
	decodedOptions, err := transformClassifiedOptions(connector, options, false)
	if err != nil {
		return "", nil, err
	}
	decodedPassphrase, err := decodeSecret(passphrase)
	if err != nil {
		return "", nil, err
	}
	return string(decodedPassphrase), decodedOptions, nil
}
