package runtimeendpoint

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/google/uuid"
)

const ClientUUIDHeader = "X-Replicaro-Client-UUID"

var clientUUIDMeta = regexp.MustCompile(`<meta name="replicaro-client-uuid" content="([0-9a-f-]{36})">`)

// ReadClientUUID reads the same public installation metadata as the browser.
// It does not authenticate the OS account and is never an activation token.
func ReadClientUUID(ctx context.Context, client *http.Client, endpoint Endpoint) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String()+"/", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("installation metadata returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	matches := clientUUIDMeta.FindAllSubmatch(body, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("installation metadata is missing or ambiguous")
	}
	value := string(matches[0][1])
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return "", fmt.Errorf("installation UUID is invalid")
	}
	return value, nil
}
