package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestResolveWorkshopCollectionPostsValveForm(t *testing.T) {
	client := &http.Client{Transport: agentRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != steamCollectionDetailsURL {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("collectioncount") != "1" || request.Form.Get("publishedfileids[0]") != "123" {
			t.Fatalf("unexpected form: %#v", request.Form)
		}
		body := `{"response":{"result":1,"collectiondetails":[{"publishedfileid":"123","result":1,"children":[{"publishedfileid":"10"},{"publishedfileid":"20"},{"publishedfileid":"10"}]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	isCollection, items, err := resolveWorkshopCollectionWithClient(context.Background(), "123", client)
	if err != nil {
		t.Fatal(err)
	}
	if !isCollection || len(items) != 2 || items[0] != "10" || items[1] != "20" {
		t.Fatalf("unexpected collection result: %v %#v", isCollection, items)
	}
}

func TestParseWorkshopCollectionDistinguishesOrdinaryItemAndEnforcesLimit(t *testing.T) {
	ordinary := []byte(`{"response":{"collectiondetails":[{"publishedfileid":"123","result":9}]}}`)
	isCollection, items, err := parseWorkshopCollectionResponse("123", ordinary)
	if err != nil || isCollection || len(items) != 0 {
		t.Fatalf("ordinary item was not preserved: collection=%v items=%#v err=%v", isCollection, items, err)
	}

	children := make([]string, 0, maxWorkshopCollectionItems+1)
	for index := 0; index <= maxWorkshopCollectionItems; index++ {
		children = append(children, `{"publishedfileid":"`+strings.Repeat("1", index%19+1)+`"}`)
	}
	overLimit := []byte(`{"response":{"collectiondetails":[{"publishedfileid":"123","result":1,"children":[` + strings.Join(children, ",") + `]}]}}`)
	isCollection, _, err = parseWorkshopCollectionResponse("123", overLimit)
	if !isCollection || err == nil || !strings.Contains(err.Error(), "item limit") {
		t.Fatalf("expected collection limit failure, got collection=%v err=%v", isCollection, err)
	}
}

type workshopSteamOperations struct {
	files  map[string][]string
	errors map[string]error
	calls  []string
}

func (*workshopSteamOperations) Install(context.Context, string, int, bool, func(int, string)) error {
	return nil
}

func (steam *workshopSteamOperations) WorkshopItem(_ context.Context, itemID string, _ func(int, string)) ([]string, error) {
	steam.calls = append(steam.calls, itemID)
	if err := steam.errors[itemID]; err != nil {
		return nil, err
	}
	return steam.files[itemID], nil
}

func (*workshopSteamOperations) PublicBuildID(context.Context, int) (string, error) {
	return "", nil
}

func (*workshopSteamOperations) InstalledBuildID(string, int) (string, string, error) {
	return "", "", nil
}

func TestInstallWorkshopCollectionDownloadsEveryItemBeforeReportingErrors(t *testing.T) {
	executor, _ := newWorkshopTestExecutor(t)
	steam := &workshopSteamOperations{errors: map[string]error{"10": errors.New("private item"), "20": errors.New("deleted item")}}
	executor.steam = steam
	executor.workshopCollectionResolver = func(context.Context, string) (bool, []string, error) {
		return true, []string{"10", "20"}, nil
	}
	_, err := executor.installWorkshop(context.Background(), domain.Job{ID: "job", InstanceID: "instance"}, "123", func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "10: private item") || !strings.Contains(err.Error(), "20: deleted item") {
		t.Fatalf("expected per-item errors, got %v", err)
	}
	if len(steam.calls) != 2 {
		t.Fatalf("expected all collection items to be attempted, got %#v", steam.calls)
	}
}

func TestInstallWorkshopFallsBackToOrdinaryItemAndInstallsValidatedVPK(t *testing.T) {
	executor, instancePath := newWorkshopTestExecutor(t)
	vpk := filepath.Join(t.TempDir(), "campaign.vpk")
	writeTestVPK(t, vpk)
	steam := &workshopSteamOperations{files: map[string][]string{"123": {vpk}}, errors: map[string]error{}}
	executor.steam = steam
	executor.workshopCollectionResolver = func(context.Context, string) (bool, []string, error) {
		return false, nil, errors.New("Valve API unavailable")
	}
	result, err := executor.installWorkshop(context.Background(), domain.Job{ID: "job", InstanceID: "instance"}, "123", func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["collection"] != false || result["collection_lookup_warning"] == nil {
		t.Fatalf("unexpected fallback result: %#v", result)
	}
	destination := filepath.Join(instancePath, "left4dead2", "addons", "123_campaign.vpk")
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("VPK was not installed: %v", err)
	}
}

func TestInstallWorkshopDoesNotRaceOverwriteExistingTarget(t *testing.T) {
	executor, instancePath := newWorkshopTestExecutor(t)
	vpk := filepath.Join(t.TempDir(), "campaign.vpk")
	writeTestVPK(t, vpk)
	destination := filepath.Join(instancePath, "left4dead2", "addons", "123_campaign.vpk")
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("existing-addon"), 0o640); err != nil {
		t.Fatal(err)
	}
	executor.steam = &workshopSteamOperations{files: map[string][]string{"123": {vpk}}, errors: map[string]error{}}
	executor.workshopCollectionResolver = func(context.Context, string) (bool, []string, error) { return false, nil, nil }
	if _, err := executor.installWorkshop(context.Background(), domain.Job{ID: "job", InstanceID: "instance"}, "123", func(int, string) {}); err == nil {
		t.Fatal("Workshop install overwrote an existing destination")
	}
	if raw, err := os.ReadFile(destination); err != nil || string(raw) != "existing-addon" {
		t.Fatalf("existing Workshop target changed: %q, %v", raw, err)
	}
}

func newWorkshopTestExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	instancePath := filepath.Join(root, "instance")
	if err := os.MkdirAll(instancePath, 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "instance", RootIndex: 0, RelativePath: "instance"}); err != nil {
		t.Fatal(err)
	}
	return &Executor{paths: paths, registry: registry, logger: log.New(io.Discard, "", 0), locks: newKeyedLocker()}, instancePath
}

func writeTestVPK(t *testing.T, path string) {
	t.Helper()
	raw := validTestVPK([]byte("workshop"), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
