package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testMapVPKEntry struct {
	path string
	data []byte
}

func TestBuildMapVPKsUsesStableLaterWinnerAndWarnsForMissingDictionary(t *testing.T) {
	inputRoot := t.TempDir()
	first := filepath.Join(inputRoot, "a.vpk")
	second := filepath.Join(inputRoot, "b.vpk")
	writeTestMapVPK(t, first, []testMapVPKEntry{
		{path: "maps/campaign.bsp", data: []byte("first BSP with stringtable_dictionary.dct")},
		{path: "scripts/shared.txt", data: []byte("first")},
		{path: "materials/removed.vtf", data: []byte("texture")},
	})
	writeTestMapVPK(t, second, []testMapVPKEntry{
		{path: "maps/campaign.bsp", data: []byte("winning BSP without a dictionary")},
		{path: "scripts/shared.txt", data: []byte("second")},
		{path: "notes/readme", data: []byte("extensionless")},
	})
	inputs := []mapVPKInput{{Path: first, SortKey: "a.vpk"}, {Path: second, SortKey: "b.vpk"}}

	firstBuild, err := buildMapVPKs(context.Background(), t.TempDir(), inputs, 0, "", "share.zip")
	if err != nil {
		t.Fatal(err)
	}
	secondBuild, err := buildMapVPKs(context.Background(), t.TempDir(), inputs, 0, "", "share.zip")
	if err != nil {
		t.Fatal(err)
	}
	if firstBuild.AggregateSHA256 != secondBuild.AggregateSHA256 {
		t.Fatalf("non-deterministic aggregate hashes: %s != %s", firstBuild.AggregateSHA256, secondBuild.AggregateSHA256)
	}
	if firstBuild.Name != "campaign" || len(firstBuild.BSPs) != 1 || firstBuild.BSPs[0].DictionaryPresent {
		t.Fatalf("unexpected BSP projection: name=%q bsps=%+v", firstBuild.Name, firstBuild.BSPs)
	}
	if firstBuild.RemovedFiles != 2 || firstBuild.RemovedBytes != int64(len("texture")+len("extensionless")) {
		t.Fatalf("unexpected cleanup statistics: files=%d bytes=%d", firstBuild.RemovedFiles, firstBuild.RemovedBytes)
	}
	if !hasMapWarning(firstBuild.Warnings, "missing_stringtable_dictionary") {
		t.Fatalf("missing dictionary warning was not retained: %+v", firstBuild.Warnings)
	}
	if got := readMapVPKEntry(t, firstBuild.Outputs[0], "scripts/shared.txt"); string(got) != "second" {
		t.Fatalf("later VPK did not win, got %q", got)
	}
}

func TestMapArchivePathRejectsTraversalDeviceNamesAndADS(t *testing.T) {
	for _, value := range []string{"../escape.vpk", "/absolute.vpk", "nested\\escape.vpk", "CON.txt", "safe/file.txt:stream", "safe//file.vpk"} {
		if _, err := validateMapArchivePath(value); err == nil {
			t.Errorf("unsafe archive path %q was accepted", value)
		}
	}
	if got, err := validateMapArchivePath("safe/nested/map.vpk"); err != nil || got != "safe/nested/map.vpk" {
		t.Fatalf("safe archive path rejected: got=%q err=%v", got, err)
	}
}

func TestMapArchiveRejectsLinksEncryptionAndFourthNestedLayer(t *testing.T) {
	root := t.TempDir()
	linked := filepath.Join(root, "linked.zip")
	var linkedBody bytes.Buffer
	linkedWriter := zip.NewWriter(&linkedBody)
	header := &zip.FileHeader{Name: "linked.vpk", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0o777)
	entry, err := linkedWriter.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("target")); err != nil {
		t.Fatal(err)
	}
	if err := linkedWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linked, linkedBody.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractMapZIP(context.Background(), linked, filepath.Join(root, "linked-output"), &mapArchiveState{Root: root}); err == nil {
		t.Fatal("ZIP symlink entry was accepted")
	}

	encrypted := filepath.Join(root, "encrypted.zip")
	var encryptedBody bytes.Buffer
	encryptedWriter := zip.NewWriter(&encryptedBody)
	plain, err := encryptedWriter.Create("map.vpk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := encryptedWriter.Close(); err != nil {
		t.Fatal(err)
	}
	raw := encryptedBody.Bytes()
	if len(raw) < 10 || !bytes.Equal(raw[:4], []byte("PK\x03\x04")) {
		t.Fatal("test ZIP local header is invalid")
	}
	binary.LittleEndian.PutUint16(raw[6:8], binary.LittleEndian.Uint16(raw[6:8])|1)
	central := bytes.Index(raw, []byte("PK\x01\x02"))
	if central < 0 {
		t.Fatal("test ZIP central directory is missing")
	}
	binary.LittleEndian.PutUint16(raw[central+8:central+10], binary.LittleEndian.Uint16(raw[central+8:central+10])|1)
	if err := os.WriteFile(encrypted, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractMapZIP(context.Background(), encrypted, filepath.Join(root, "encrypted-output"), &mapArchiveState{Root: root}); err == nil {
		t.Fatal("encrypted ZIP was accepted")
	}

	fourth := filepath.Join(root, "fourth.zip")
	if err := os.WriteFile(fourth, []byte("PK\x03\x04"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discoverMapFile(context.Background(), root, fourth, "a.zip!/b.zip!/c.zip!/d.zip", maxMapArchiveDepth, &mapArchiveState{Root: root}); err == nil {
		t.Fatal("fourth nested archive layer was accepted")
	}
}

func TestOpenMapVPKReadsV2ExternalDataBlock(t *testing.T) {
	root := t.TempDir()
	data := []byte("BSP payload with stringtable_dictionary.dct")
	tree := testExternalMapVPKTree("maps", "external_map", "bsp", data, 0)
	header := make([]byte, 28)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 2)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	directoryPath := filepath.Join(root, "external_dir.vpk")
	if err := os.WriteFile(directoryPath, append(header, tree...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "external_000.vpk"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openMapVPK(directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.entries) != 1 || source.entries[0].archiveIndex != 0 {
		t.Fatalf("unexpected external VPK entries: %+v", source.entries)
	}
	entry := source.entries[0]
	entry.source = source
	checksum, _, dictionaryPresent, err := inspectMapVPKEntry(context.Background(), entry, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if checksum != crc32.ChecksumIEEE(data) || !dictionaryPresent {
		t.Fatalf("external VPK validation checksum=%08x dictionary=%v", checksum, dictionaryPresent)
	}
}

func TestPartitionMapVPKEntriesSplitsOnlyAtFileBoundaries(t *testing.T) {
	entries := []mapPreparedEntry{
		{entry: mapVPKEntry{path: "maps/first.bsp"}, size: 512 << 20},
		{entry: mapVPKEntry{path: "maps/second.bsp"}, size: 512 << 20},
		{entry: mapVPKEntry{path: "maps/third.bsp"}, size: 512 << 20},
		{entry: mapVPKEntry{path: "scripts/small.txt"}, size: 1 << 20},
	}
	groups, err := partitionMapVPKEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || len(groups[0]) != 2 || len(groups[1]) != 2 || groups[1][0].entry.path != "maps/third.bsp" {
		t.Fatalf("unexpected VPK partition: %+v", groups)
	}
}

func writeTestMapVPK(t *testing.T, path string, entries []testMapVPKEntry) {
	t.Helper()
	prepared := make([]mapPreparedEntry, len(entries))
	for index, entry := range entries {
		prepared[index] = mapPreparedEntry{
			entry: mapVPKEntry{path: entry.path, size: int64(len(entry.data))},
			crc:   crc32.ChecksumIEEE(entry.data),
			size:  int64(len(entry.data)),
		}
	}
	tree, err := buildMapVPKTree(prepared)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	data := append(header, tree...)
	for _, entry := range entries {
		data = append(data, entry.data...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testExternalMapVPKTree(directory, name, extension string, data []byte, archiveIndex uint16) []byte {
	tree := make([]byte, 0, len(directory)+len(name)+len(extension)+24)
	tree = append(tree, extension...)
	tree = append(tree, 0)
	tree = append(tree, directory...)
	tree = append(tree, 0)
	tree = append(tree, name...)
	tree = append(tree, 0)
	header := make([]byte, 18)
	binary.LittleEndian.PutUint32(header[:4], crc32.ChecksumIEEE(data))
	binary.LittleEndian.PutUint16(header[6:8], archiveIndex)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(data)))
	binary.LittleEndian.PutUint16(header[16:18], 0xffff)
	tree = append(tree, header...)
	return append(tree, 0, 0, 0)
}

func readMapVPKEntry(t *testing.T, vpkPath, virtualPath string) []byte {
	t.Helper()
	source, err := openMapVPK(vpkPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := range source.entries {
		entry := source.entries[index]
		if !strings.EqualFold(entry.path, virtualPath) {
			continue
		}
		entry.source = source
		reader, err := entry.open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		return data
	}
	t.Fatalf("VPK entry %q not found", virtualPath)
	return nil
}

func hasMapWarning(warnings []mapLocalWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}
