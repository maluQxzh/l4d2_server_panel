package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	steamCollectionDetailsURL  = "https://api.steampowered.com/ISteamRemoteStorage/GetCollectionDetails/v1/"
	steamPublishedDetailsURL   = "https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/"
	maxWorkshopCollectionItems = 200
	maxWorkshopAPIResponse     = int64(2 << 20)
	maxWorkshopDetailsResponse = int64(8 << 20)
	workshopDetailsBatchSize   = 100
)

// ResolveWorkshopCollection determines whether publishedFileID is a Workshop
// collection. The Valve endpoint does not require a publisher API key.
func ResolveWorkshopCollection(ctx context.Context, publishedFileID string) (bool, []string, error) {
	client := newWorkshopHTTPClient()
	return resolveWorkshopCollectionWithClient(ctx, publishedFileID, client)
}

func resolveWorkshopCollectionWithClient(ctx context.Context, publishedFileID string, client *http.Client) (bool, []string, error) {
	if err := validateWorkshopID(publishedFileID); err != nil {
		return false, nil, err
	}
	form := url.Values{
		"collectioncount":     {"1"},
		"publishedfileids[0]": {publishedFileID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, steamCollectionDetailsURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		return false, nil, fmt.Errorf("query Workshop collection: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, nil, fmt.Errorf("Workshop collection endpoint returned %s", response.Status)
	}
	if response.ContentLength > maxWorkshopAPIResponse {
		return false, nil, errors.New("Workshop collection response exceeds 2 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxWorkshopAPIResponse+1))
	if err != nil {
		return false, nil, err
	}
	if int64(len(raw)) > maxWorkshopAPIResponse {
		return false, nil, errors.New("Workshop collection response exceeds 2 MiB")
	}
	return parseWorkshopCollectionResponse(publishedFileID, raw)
}

func parseWorkshopCollectionResponse(publishedFileID string, raw []byte) (bool, []string, error) {
	type child struct {
		PublishedFileID string `json:"publishedfileid"`
	}
	type detail struct {
		PublishedFileID string   `json:"publishedfileid"`
		Result          int      `json:"result"`
		Children        *[]child `json:"children"`
	}
	var envelope struct {
		Response struct {
			Result            int      `json:"result"`
			CollectionDetails []detail `json:"collectiondetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, nil, errors.New("invalid Workshop collection response")
	}
	if len(envelope.Response.CollectionDetails) != 1 {
		return false, nil, errors.New("Workshop collection response did not contain one result")
	}
	detailResult := envelope.Response.CollectionDetails[0]
	if detailResult.PublishedFileID != "" && detailResult.PublishedFileID != publishedFileID {
		return false, nil, errors.New("Workshop collection response id did not match the request")
	}
	if detailResult.Result != 1 || detailResult.Children == nil {
		return false, nil, nil
	}
	if len(*detailResult.Children) == 0 {
		return true, nil, errors.New("Workshop collection contains no downloadable items")
	}
	if len(*detailResult.Children) > maxWorkshopCollectionItems {
		return true, nil, fmt.Errorf("Workshop collection exceeds the %d item limit", maxWorkshopCollectionItems)
	}
	items := make([]string, 0, len(*detailResult.Children))
	seen := make(map[string]bool, len(*detailResult.Children))
	for _, child := range *detailResult.Children {
		if err := validateWorkshopID(child.PublishedFileID); err != nil {
			return true, nil, fmt.Errorf("Workshop collection contains an invalid child id: %w", err)
		}
		if !seen[child.PublishedFileID] {
			seen[child.PublishedFileID] = true
			items = append(items, child.PublishedFileID)
		}
	}
	return true, items, nil
}

// ResolveWorkshopItemTitles retrieves the public titles used to form stable,
// human-readable addon filenames. The Valve endpoint does not require an API
// key. Requests are bounded and batched because the response also includes
// metadata that the Agent does not otherwise need.
func ResolveWorkshopItemTitles(ctx context.Context, publishedFileIDs []string) (map[string]string, error) {
	client := newWorkshopHTTPClient()
	return resolveWorkshopItemTitlesWithClient(ctx, publishedFileIDs, client)
}

// Workshop metadata uses code-owned Valve URLs only. Unlike arbitrary package
// downloads, these requests may honor the node administrator's HTTPS proxy so
// Agents in restricted networks can resolve collection membership and titles.
// Redirects remain disabled and TLS certificate validation remains enabled.
func newWorkshopHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.IdleConnTimeout = 20 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Workshop metadata endpoint must not redirect")
		},
	}
}

func resolveWorkshopItemTitlesWithClient(ctx context.Context, publishedFileIDs []string, client *http.Client) (map[string]string, error) {
	if len(publishedFileIDs) == 0 || len(publishedFileIDs) > maxWorkshopCollectionItems {
		return nil, errors.New("Workshop item count is invalid")
	}
	titles := make(map[string]string, len(publishedFileIDs))
	seen := make(map[string]bool, len(publishedFileIDs))
	for _, itemID := range publishedFileIDs {
		if err := validateWorkshopID(itemID); err != nil {
			return nil, err
		}
		if seen[itemID] {
			return nil, errors.New("Workshop item ids must be unique")
		}
		seen[itemID] = true
	}
	for start := 0; start < len(publishedFileIDs); start += workshopDetailsBatchSize {
		end := start + workshopDetailsBatchSize
		if end > len(publishedFileIDs) {
			end = len(publishedFileIDs)
		}
		batch, err := queryWorkshopItemTitles(ctx, publishedFileIDs[start:end], client)
		if err != nil {
			return nil, err
		}
		for itemID, title := range batch {
			titles[itemID] = title
		}
	}
	return titles, nil
}

func queryWorkshopItemTitles(ctx context.Context, publishedFileIDs []string, client *http.Client) (map[string]string, error) {
	form := url.Values{"itemcount": {fmt.Sprintf("%d", len(publishedFileIDs))}}
	requested := make(map[string]bool, len(publishedFileIDs))
	for index, itemID := range publishedFileIDs {
		form.Set(fmt.Sprintf("publishedfileids[%d]", index), itemID)
		requested[itemID] = true
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, steamPublishedDetailsURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Workshop item details: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Workshop item details endpoint returned %s", response.Status)
	}
	if response.ContentLength > maxWorkshopDetailsResponse {
		return nil, errors.New("Workshop item details response exceeds 8 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxWorkshopDetailsResponse+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxWorkshopDetailsResponse {
		return nil, errors.New("Workshop item details response exceeds 8 MiB")
	}
	return parseWorkshopItemTitlesResponse(requested, raw)
}

func parseWorkshopItemTitlesResponse(requested map[string]bool, raw []byte) (map[string]string, error) {
	type detail struct {
		PublishedFileID string `json:"publishedfileid"`
		Result          int    `json:"result"`
		ConsumerAppID   int    `json:"consumer_app_id"`
		Title           string `json:"title"`
	}
	var envelope struct {
		Response struct {
			PublishedFileDetails []detail `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("invalid Workshop item details response")
	}
	titles := make(map[string]string, len(requested))
	for _, item := range envelope.Response.PublishedFileDetails {
		if !requested[item.PublishedFileID] {
			return nil, errors.New("Workshop item details response contained an unexpected id")
		}
		if _, exists := titles[item.PublishedFileID]; exists {
			return nil, errors.New("Workshop item details response contained a duplicate id")
		}
		if item.Result != 1 {
			return nil, fmt.Errorf("Workshop item %s is unavailable", item.PublishedFileID)
		}
		if item.ConsumerAppID != 550 {
			return nil, fmt.Errorf("Workshop item %s does not belong to Left 4 Dead 2", item.PublishedFileID)
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			return nil, fmt.Errorf("Workshop item %s has no title", item.PublishedFileID)
		}
		titles[item.PublishedFileID] = title
	}
	for itemID := range requested {
		if _, ok := titles[itemID]; !ok {
			return nil, fmt.Errorf("Workshop item details response omitted %s", itemID)
		}
	}
	return titles, nil
}

func validateWorkshopID(itemID string) error {
	if itemID == "" || len(itemID) > 20 {
		return errors.New("invalid Workshop item id")
	}
	for _, r := range itemID {
		if r < '0' || r > '9' {
			return errors.New("Workshop item id must be numeric")
		}
	}
	return nil
}
