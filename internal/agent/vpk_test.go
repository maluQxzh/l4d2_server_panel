package agent

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateVPKParsesTreeBoundsAndCRC(t *testing.T) {
	data := []byte("test-data")
	tree := minimalVPKTree("scripts", "panel", "txt", data, 0xffff)
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	path := filepath.Join(t.TempDir(), "valid.vpk")
	if err := os.WriteFile(path, append(append(header, tree...), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if version, hash, err := validateVPK(path); err != nil || version != 1 || len(hash) != 64 {
		t.Fatalf("validateVPK = version %d hash %q err %v", version, hash, err)
	}

	badTree := minimalVPKTree("../escape", "panel", "txt", data, 0xffff)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(badTree)))
	badPath := filepath.Join(t.TempDir(), "escape.vpk")
	if err := os.WriteFile(badPath, append(append(header, badTree...), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateVPK(badPath); err == nil {
		t.Fatal("unsafe VPK path was accepted")
	}
}

func TestValidateVPKRejectsTruncatedHeaderAndBadTerminator(t *testing.T) {
	short := filepath.Join(t.TempDir(), "short.vpk")
	if err := os.WriteFile(short, []byte{0x34, 0x12, 0xaa, 0x55, 1, 0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateVPK(short); err == nil {
		t.Fatal("truncated VPK was accepted")
	}
	data := []byte("x")
	tree := minimalVPKTree(" ", "panel", "txt", data, 0)
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	path := filepath.Join(t.TempDir(), "terminator.vpk")
	if err := os.WriteFile(path, append(append(header, tree...), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateVPK(path); err == nil {
		t.Fatal("invalid entry terminator was accepted")
	}
}

func TestValidateVPKRejectsOversizedLocalFileBeforeParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.vpk")
	if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxPackageDownload+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateVPK(path); err == nil {
		t.Fatal("oversized local VPK was accepted")
	}
}

func minimalVPKTree(directory, name, extension string, data []byte, terminator uint16) []byte {
	tree := make([]byte, 0)
	tree = append(tree, extension...)
	tree = append(tree, 0)
	tree = append(tree, directory...)
	tree = append(tree, 0)
	tree = append(tree, name...)
	tree = append(tree, 0)
	entry := make([]byte, 18)
	binary.LittleEndian.PutUint32(entry[:4], crc32.ChecksumIEEE(data))
	binary.LittleEndian.PutUint16(entry[6:8], vpkEmbeddedArchive)
	binary.LittleEndian.PutUint32(entry[12:16], uint32(len(data)))
	binary.LittleEndian.PutUint16(entry[16:18], terminator)
	tree = append(tree, entry...)
	return append(tree, 0, 0, 0)
}

func validTestVPK(data []byte, version uint32) []byte {
	tree := minimalVPKTree("scripts", "panel", "txt", data, 0xffff)
	headerSize := 12
	if version == 2 {
		headerSize = 28
	}
	header := make([]byte, headerSize)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], version)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	if version == 2 {
		binary.LittleEndian.PutUint32(header[12:16], uint32(len(data)))
	}
	return append(append(header, tree...), data...)
}
