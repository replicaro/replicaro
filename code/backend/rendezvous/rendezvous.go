package rendezvous

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/runtimeendpoint"
)

const (
	RecordVersion     = "replicaro-rendezvous-v1"
	ActivationVersion = "replicaro-activation-v1"
	FileName          = "replicaro.runtime.json"
	ActivationPath    = "/api/activate"
	maxRecordBytes    = 4096
	maxActivationBody = 1024
)

type Record struct {
	Version string `json:"version"`
	Nonce   string `json:"nonce"`
	PID     int    `json:"pid"`
	URL     string `json:"url"`
	Token   string `json:"activationToken"`
}

type ActivationRequest struct {
	Version string `json:"version"`
	Nonce   string `json:"nonce"`
	PID     int    `json:"pid"`
}

func NewRecord(endpoint runtimeendpoint.Endpoint) (Record, error) {
	nonce, err := randomSecret()
	if err != nil {
		return Record{}, err
	}
	token, err := randomSecret()
	if err != nil {
		return Record{}, err
	}
	record := Record{
		Version: RecordVersion, Nonce: nonce, PID: os.Getpid(),
		URL: endpoint.String(), Token: token,
	}
	return record, record.validate()
}

func PrepareOwner(path string) error {
	record, err := read(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refuse unsafe stale rendezvous state: %w", err)
	}
	return removeMatching(path, record.Nonce)
}

func Publish(path string, record Record) error {
	if err := record.validate(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("rendezvous destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".replicaro-runtime-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := appdata.SecurePath(temporary, false); err != nil {
		return err
	}
	if err := validateOwnerOnly(temporary); err != nil {
		return err
	}
	if err := appdata.AtomicReplace(temporary, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func RemoveIfNonce(path, nonce string) error {
	if !validSecret(nonce) {
		return fmt.Errorf("rendezvous nonce is invalid")
	}
	record, err := read(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(record.Nonce), []byte(nonce)) != 1 {
		return nil
	}
	return removeMatching(path, nonce)
}

func removeMatching(path, nonce string) error {
	record, err := read(path, false)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(record.Nonce), []byte(nonce)) != 1 {
		return nil
	}
	return os.Remove(path)
}

func ReadForActivation(path string) (Record, error) {
	return read(path, true)
}

func read(path string, requireLivePID bool) (Record, error) {
	if err := validateOwnerOnly(path); err != nil {
		return Record{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return Record{}, err
	}
	if len(data) > maxRecordBytes {
		return Record{}, fmt.Errorf("rendezvous record exceeds limit")
	}
	var record Record
	if err := decodeStrict(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode rendezvous record: %w", err)
	}
	if err := record.validate(); err != nil {
		return Record{}, err
	}
	if requireLivePID && !processAlive(record.PID) {
		return Record{}, fmt.Errorf("rendezvous process is not active")
	}
	return record, nil
}

func Activate(ctx context.Context, path string, retryFor time.Duration) error {
	if retryFor <= 0 {
		retryFor = 750 * time.Millisecond
	}
	deadline := time.Now().Add(retryFor)
	var record Record
	var err error
	for {
		record, err = ReadForActivation(path)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	body, err := json.Marshal(ActivationRequest{
		Version: ActivationVersion, Nonce: record.Nonce, PID: record.PID,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, record.URL+ActivationPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+record.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Host = strings.TrimPrefix(record.URL, "http://")
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("activation redirect is not permitted")
		},
	}
	endpoint, err := runtimeendpoint.ParseExact(record.URL)
	if err != nil {
		return err
	}
	clientUUID, err := runtimeendpoint.ReadClientUUID(ctx, client, endpoint)
	if err != nil {
		return fmt.Errorf("read existing Replicaro installation: %w", err)
	}
	request.Header.Set(runtimeendpoint.ClientUUIDHeader, clientUUID)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("activate existing Replicaro instance: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxActivationBody))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("existing Replicaro instance rejected activation")
	}
	return nil
}

func Handler(record Record, activate func() error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method is not allowed", http.StatusMethodNotAllowed)
			return
		}
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			http.Error(w, "Content-Type application/json is required", http.StatusUnsupportedMediaType)
			return
		}
		if request.ContentLength < 0 || request.ContentLength > maxActivationBody {
			http.Error(w, "activation body is invalid", http.StatusBadRequest)
			return
		}
		authorizations := request.Header.Values("Authorization")
		authorization := request.Header.Get("Authorization")
		expected := "Bearer " + record.Token
		if len(authorizations) != 1 || len(authorization) != len(expected) ||
			subtle.ConstantTimeCompare([]byte(authorization), []byte(expected)) != 1 {
			http.Error(w, "activation authentication failed", http.StatusUnauthorized)
			return
		}
		request.Body = http.MaxBytesReader(w, request.Body, maxActivationBody)
		var activation ActivationRequest
		if err := decodeStrictReader(request.Body, &activation); err != nil ||
			activation.Version != ActivationVersion || activation.PID != record.PID ||
			len(activation.Nonce) != len(record.Nonce) ||
			subtle.ConstantTimeCompare([]byte(activation.Nonce), []byte(record.Nonce)) != 1 {
			http.Error(w, "activation body is invalid", http.StatusBadRequest)
			return
		}
		if activate == nil || activate() != nil {
			http.Error(w, "activation failed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (record Record) validate() error {
	if record.Version != RecordVersion || !validSecret(record.Nonce) ||
		!validSecret(record.Token) || record.PID <= 0 {
		return fmt.Errorf("rendezvous record fields are invalid")
	}
	endpoint, err := runtimeendpoint.ParseExact(record.URL)
	if err != nil || endpoint.String() != record.URL {
		return fmt.Errorf("rendezvous URL is invalid")
	}
	return nil
}

func randomSecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate rendezvous secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validSecret(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func decodeStrict(data []byte, target any) error {
	return decodeStrictReader(bytes.NewReader(data), target)
}

func decodeStrictReader(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
