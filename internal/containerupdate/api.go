package containerupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const dockerAPIVersion = "v1.43"

type dockerClient struct {
	socket string
	http   *http.Client
}

type dockerVersion struct {
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	OS            string `json:"Os"`
	Arch          string `json:"Arch"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

type containerInspect struct {
	ID              string         `json:"Id"`
	Name            string         `json:"Name"`
	Image           string         `json:"Image"`
	Config          map[string]any `json:"Config"`
	HostConfig      map[string]any `json:"HostConfig"`
	Mounts          []dockerMount  `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]map[string]any `json:"Networks"`
	} `json:"NetworkSettings"`
	State struct {
		Running bool `json:"Running"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

type imageInspect struct {
	ID           string `json:"Id"`
	Architecture string `json:"Architecture"`
	OS           string `json:"Os"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func newDockerClient(socket string) (*dockerClient, error) {
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Docker endpoint must be a Unix socket and not a symlink")
	}
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
		},
	}
	return &dockerClient{socket: socket, http: &http.Client{Transport: transport, Timeout: 10 * time.Minute}}, nil
}

func (c *dockerClient) ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

func (c *dockerClient) version(ctx context.Context) (dockerVersion, error) {
	var result dockerVersion
	err := c.do(ctx, http.MethodGet, "/version", nil, nil, &result)
	return result, err
}

func (c *dockerClient) inspectContainer(ctx context.Context, id string) (containerInspect, error) {
	var result containerInspect
	err := c.do(ctx, http.MethodGet, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(id)+"/json", nil, nil, &result)
	return result, err
}

func (c *dockerClient) inspectImage(ctx context.Context, reference string) (imageInspect, error) {
	var result imageInspect
	err := c.do(ctx, http.MethodGet, "/"+dockerAPIVersion+"/images/"+url.PathEscape(reference)+"/json", nil, nil, &result)
	return result, err
}

func (c *dockerClient) loadImage(ctx context.Context, input io.Reader) error {
	query := url.Values{"quiet": []string{"1"}}
	return c.doWithContentType(ctx, http.MethodPost, "/"+dockerAPIVersion+"/images/load", query, input, nil, "application/x-tar")
}

func (c *dockerClient) tagImage(ctx context.Context, id, repository, tag string) error {
	query := url.Values{"repo": []string{repository}, "tag": []string{tag}}
	return c.do(ctx, http.MethodPost, "/"+dockerAPIVersion+"/images/"+url.PathEscape(id)+"/tag", query, nil, nil)
}

func (c *dockerClient) createContainer(ctx context.Context, name string, request map[string]any) (string, error) {
	query := url.Values{"name": []string{name}}
	var response struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/create", query, request, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", errors.New("Docker create returned an empty container ID")
	}
	return response.ID, nil
}

func (c *dockerClient) startContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (c *dockerClient) stopContainer(ctx context.Context, id string, seconds int) error {
	query := url.Values{"t": []string{fmt.Sprintf("%d", seconds)}}
	return c.do(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(id)+"/stop", query, nil, nil)
}

func (c *dockerClient) renameContainer(ctx context.Context, id, name string) error {
	query := url.Values{"name": []string{name}}
	return c.do(ctx, http.MethodPost, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(id)+"/rename", query, nil, nil)
}

func (c *dockerClient) removeContainer(ctx context.Context, id string, force bool) error {
	query := url.Values{"force": []string{fmt.Sprintf("%t", force)}, "v": []string{"0"}}
	return c.do(ctx, http.MethodDelete, "/"+dockerAPIVersion+"/containers/"+url.PathEscape(id), query, nil, nil)
}

func (c *dockerClient) execReady(ctx context.Context, containerID string) error {
	request := map[string]any{
		"AttachStdout": false, "AttachStderr": false, "Tty": false,
		"Cmd": []string{"wget", "-qO-", "http://127.0.0.1:19090/readyz"},
	}
	var created struct {
		ID string `json:"Id"`
	}
	path := "/" + dockerAPIVersion + "/containers/" + url.PathEscape(containerID) + "/exec"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, request, &created); err != nil {
		return err
	}
	if created.ID == "" {
		return errors.New("Docker exec returned an empty ID")
	}
	if err := c.doJSON(ctx, http.MethodPost, "/"+dockerAPIVersion+"/exec/"+url.PathEscape(created.ID)+"/start", nil,
		map[string]any{"Detach": true, "Tty": false}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := c.do(ctx, http.MethodGet, "/"+dockerAPIVersion+"/exec/"+url.PathEscape(created.ID)+"/json", nil, nil, &status); err != nil {
			return err
		}
		if !status.Running {
			if status.ExitCode != 0 {
				return fmt.Errorf("readiness command exited with code %d", status.ExitCode)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("readiness command did not finish")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *dockerClient) doJSON(ctx context.Context, method, path string, query url.Values, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	return c.do(ctx, method, path, query, body, output)
}

func (c *dockerClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader, output any) error {
	return c.doWithContentType(ctx, method, path, query, body, output, "application/json")
}

func (c *dockerClient) doWithContentType(ctx context.Context, method, path string, query url.Values, body io.Reader, output any, contentType string) error {
	if query != nil && len(query) > 0 {
		path += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("Docker API %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 4<<20)
	if (response.StatusCode < 200 || response.StatusCode >= 300) && response.StatusCode != http.StatusNotModified {
		data, _ := io.ReadAll(limited)
		var daemonError struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &daemonError)
		message := strings.TrimSpace(daemonError.Message)
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("Docker API %s %s returned %s: %s", method, path, response.Status, message)
	}
	if output != nil {
		if err := json.NewDecoder(limited).Decode(output); err != nil {
			return fmt.Errorf("decode Docker API response: %w", err)
		}
		return nil
	}
	_, err = io.Copy(io.Discard, limited)
	return err
}
