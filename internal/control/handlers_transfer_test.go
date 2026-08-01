package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateChunkRequestAppliesStrictDefaultAndLimit(t *testing.T) {
	request := chunkRequest{}
	response := httptest.NewRecorder()
	if !validateChunkRequest(response, &request, true) {
		t.Fatalf("default chunk request rejected: %s", response.Body.String())
	}
	if request.Length != maxHTTPTransferChunk {
		t.Fatalf("default length = %d", request.Length)
	}

	request = chunkRequest{Length: maxHTTPTransferChunk + 1}
	response = httptest.NewRecorder()
	if validateChunkRequest(response, &request, true) || response.Code != 400 {
		t.Fatalf("oversized request was accepted: code=%d", response.Code)
	}
}

func TestSafeDownloadNameRemovesHeaderAndPathCharacters(t *testing.T) {
	name := safeDownloadName("../bad\r\n\x00/name.zip")
	if strings.ContainsAny(name, "\r\n\x00/\\") || name == "" {
		t.Fatalf("unsafe download name: %q", name)
	}
}

func TestTransferPollPath(t *testing.T) {
	if got := transferPollPath("/api/v1/instances/i/logs/search", "job-1"); got != "/api/v1/instances/i/logs/search/job-1" {
		t.Fatalf("poll path = %q", got)
	}
}
