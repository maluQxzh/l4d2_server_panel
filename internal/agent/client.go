package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/version"
)

type enrollRequest struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
	InstanceRoot string `json:"instance_root"`
}

type enrollResponse struct {
	NodeID     string `json:"node_id"`
	Credential string `json:"credential"`
}

func Enroll(ctx context.Context, configPath string, config *Config) error {
	if config.Credential != "" && config.NodeID != "" {
		return nil
	}
	if config.EnrollmentToken == "" {
		return errors.New("enrollment_token is required when credential is empty")
	}
	endpoint, err := config.endpoint("/api/v1/agent/enroll", false)
	if err != nil {
		return err
	}
	request := enrollRequest{Token: config.EnrollmentToken, Name: config.NodeName, OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: version.Version, InstanceRoot: config.InstanceRoots[0]}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.InsecureSkipVerify} //nolint:gosec -- explicit operator setting for private CAs
	client := &http.Client{Timeout: 20 * time.Second, Transport: transport}
	response, err := client.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("enroll request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("enrollment rejected (%s): %s", response.Status, strings.TrimSpace(string(body)))
	}
	var enrolled enrollResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&enrolled); err != nil {
		return fmt.Errorf("decode enrollment response: %w", err)
	}
	if enrolled.NodeID == "" || enrolled.Credential == "" {
		return errors.New("enrollment response omitted node_id or credential")
	}
	config.NodeID, config.Credential, config.EnrollmentToken = enrolled.NodeID, enrolled.Credential, ""
	if err := SaveConfig(configPath, config); err != nil {
		config.NodeID, config.Credential = "", ""
		return fmt.Errorf("persist enrolled credential: %w", err)
	}
	return nil
}
