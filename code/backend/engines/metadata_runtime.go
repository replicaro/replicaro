package engines

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed metadata.json
var embeddedEngineMetadata []byte

type runtimeArtifact struct {
	BinarySHA256 string `json:"binarySha256"`
}
type runtimeEngine struct {
	ID        string                     `json:"id"`
	Artifacts map[string]runtimeArtifact `json:"artifacts"`
}
type runtimeMetadata struct {
	Engines []runtimeEngine `json:"engines"`
}

var runtimeMetadataOnce sync.Once
var runtimeMetadataValue runtimeMetadata

func expectedEmbeddedHash(id, target string) (string, error) {
	runtimeMetadataOnce.Do(func() { _ = json.Unmarshal(embeddedEngineMetadata, &runtimeMetadataValue) })
	for _, engine := range runtimeMetadataValue.Engines {
		if engine.ID == id {
			if artifact, ok := engine.Artifacts[target]; ok && artifact.BinarySHA256 != "" {
				return artifact.BinarySHA256, nil
			}
		}
	}
	return "", fmt.Errorf("no embedded hash metadata for %s/%s", id, target)
}
