package agent

import (
	"context"
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
	maxWorkshopCollectionItems = 200
	maxWorkshopAPIResponse     = int64(2 << 20)
)

// ResolveWorkshopCollection determines whether publishedFileID is a Workshop
// collection. The Valve endpoint does not require a publisher API key.
func ResolveWorkshopCollection(ctx context.Context, publishedFileID string) (bool, []string, error) {
	client := newRestrictedHTTPClient(map[string]bool{"api.steampowered.com": true}, 15*time.Second, 0)
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
