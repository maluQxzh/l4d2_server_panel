package agent

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

func TestDecodeMapPathBytesSupportsExplicitLegacyEncodings(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("广州地图"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, used, err := decodeMapPathBytes(gbk, domain.MapPathEncodingAuto)
	if err != nil || decoded != "广州地图" || used != domain.MapPathEncodingGBK {
		t.Fatalf("auto GBK decode = %q/%q, err=%v", decoded, used, err)
	}
	if _, _, err := decodeMapPathBytes(gbk, domain.MapPathEncodingUTF8); err == nil {
		t.Fatal("forced UTF-8 accepted GBK bytes")
	}

	big5, err := traditionalchinese.Big5.NewEncoder().Bytes([]byte("廣州地圖"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, used, err = decodeMapPathBytes(big5, domain.MapPathEncodingBig5)
	if err != nil || decoded != "廣州地圖" || used != domain.MapPathEncodingBig5 {
		t.Fatalf("explicit Big5 decode = %q/%q, err=%v", decoded, used, err)
	}

	var stats mapPathDecodeStats
	decoded, err = decodeMapZIPPath(string(gbk)+".vpk", false, domain.MapPathEncodingGBK, &stats)
	if err != nil || decoded != "广州地图.vpk" || stats.ZIPGBK != 1 {
		t.Fatalf("explicit GBK ZIP override = %q stats=%+v err=%v", decoded, stats, err)
	}
}

func TestExtractMapZIPDecodesGBKAndWritesOnlyProcessableInputs(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "map.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	gbkName, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("地图/广州副本.vpk"))
	if err != nil {
		t.Fatal(err)
	}
	header := &zip.FileHeader{Name: string(gbkName), Method: zip.Store, NonUTF8: true}
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	vpkHeader := make([]byte, 12)
	binary.LittleEndian.PutUint32(vpkHeader[:4], vpkSignature)
	if _, err := entry.Write(vpkHeader); err != nil {
		t.Fatal(err)
	}
	ignored, err := writer.Create("文档/说明.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(ignored, "not a map input"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	destination := t.TempDir()
	state := &mapArchiveState{Root: destination, SelectiveInputs: true, PathEncoding: domain.MapPathEncodingAuto,
		DiskFreeReserve: 1, MaxExpanded: 1 << 20, MaxEntrySize: 1 << 20}
	files, err := extractMapZIP(context.Background(), archivePath, destination, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Relative != "地图/广州副本.vpk" {
		t.Fatalf("selected files = %+v", files)
	}
	if state.Ignored != 1 || state.PathDecodes.ZIPGBK != 2 || state.Expanded != int64(len(vpkHeader)) {
		t.Fatalf("archive statistics = %+v", state)
	}
	if _, err := os.Stat(filepath.Join(destination, "文档", "说明.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("irrelevant file was written: %v", err)
	}
}

func TestFailedMapImportPersistsStructuredFailureResult(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "map-failure", NodeID: "node-1", Type: "map.import.qq_flash"}
	if existing, err := journal.Begin(job); err != nil || existing != nil {
		t.Fatalf("begin job: existing=%v err=%v", existing, err)
	}
	agent := &Agent{config: &Config{NodeID: "node-1"}, journal: journal, ctx: context.Background(),
		activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool), logger: log.New(io.Discard, "", 0)}
	agent.handleClaimedJob(job, func(context.Context, domain.Job, ProgressFunc) (map[string]any, error) {
		return nil, wrapMapImportFailure(mapImportStageArchive, "广州地图.zip", errors.New("decode ZIP path: invalid bytes"))
	})
	entry, ok := journal.Get(job.ID)
	if !ok || entry.Status != domain.JobFailed {
		t.Fatalf("journal entry = %+v, exists=%v", entry, ok)
	}
	failure, ok := entry.Result["failure"].(map[string]any)
	if !ok || failure["stage"] != mapImportStageArchive || failure["code"] != "map_archive_path_encoding_invalid" || failure["source"] != "广州地图.zip" {
		t.Fatalf("structured failure = %#v", entry.Result)
	}
}
