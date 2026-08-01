package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	pathpkg "path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	vpkSignature       = uint32(0x55aa1234)
	vpkEmbeddedArchive = uint16(0x7fff)
	maxVPKTreeSize     = uint32(64 << 20)
	maxVPKEntries      = 100000
)

type vpkEntry struct {
	path    string
	crc     uint32
	preload []byte
	offset  uint32
	length  uint32
}

func validateVPK(path string) (int, string, error) {
	version, hash, _, err := validateVPKDetailed(path)
	return version, hash, err
}

func validateVPKDetailed(path string) (int, string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, "", nil, errors.New("VPK must be a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxPackageDownload {
		return 0, "", nil, errors.New("VPK must contain data and cannot exceed 512 MiB")
	}
	var base [12]byte
	if _, err := io.ReadFull(f, base[:]); err != nil {
		return 0, "", nil, errors.New("VPK header is truncated")
	}
	if binary.LittleEndian.Uint32(base[:4]) != vpkSignature {
		return 0, "", nil, errors.New("invalid VPK signature")
	}
	version := int(binary.LittleEndian.Uint32(base[4:8]))
	treeSize := binary.LittleEndian.Uint32(base[8:12])
	if (version != 1 && version != 2) || treeSize == 0 || treeSize > maxVPKTreeSize {
		return 0, "", nil, errors.New("unsupported VPK version or invalid directory tree size")
	}
	headerSize := int64(12)
	fileDataSize := info.Size() - headerSize - int64(treeSize)
	var archiveMD5Size, otherMD5Size, signatureSize uint32
	if version == 2 {
		var extra [16]byte
		if _, err := io.ReadFull(f, extra[:]); err != nil {
			return 0, "", nil, errors.New("VPK v2 header is truncated")
		}
		headerSize = 28
		fileDataSize = int64(binary.LittleEndian.Uint32(extra[:4]))
		archiveMD5Size = binary.LittleEndian.Uint32(extra[4:8])
		otherMD5Size = binary.LittleEndian.Uint32(extra[8:12])
		signatureSize = binary.LittleEndian.Uint32(extra[12:16])
		sections := headerSize + int64(treeSize) + fileDataSize + int64(archiveMD5Size) + int64(otherMD5Size) + int64(signatureSize)
		if sections != info.Size() || archiveMD5Size%28 != 0 || (otherMD5Size != 0 && otherMD5Size != 48) {
			return 0, "", nil, errors.New("VPK v2 section sizes are inconsistent")
		}
	}
	if fileDataSize < 0 || headerSize+int64(treeSize) > info.Size() {
		return 0, "", nil, errors.New("VPK directory tree exceeds file bounds")
	}
	tree := make([]byte, treeSize)
	if _, err := io.ReadFull(f, tree); err != nil {
		return 0, "", nil, errors.New("VPK directory tree is truncated")
	}
	entries, err := parseVPKTree(tree, uint64(fileDataSize))
	if err != nil {
		return 0, "", nil, err
	}
	dataStart := headerSize + int64(treeSize)
	if err := verifyVPKEntryData(f, dataStart, entries); err != nil {
		return 0, "", nil, err
	}
	if version == 2 && signatureSize > 0 {
		signatureOffset := dataStart + fileDataSize + int64(archiveMD5Size) + int64(otherMD5Size)
		if err := validateVPKSignatureSection(f, signatureOffset, int64(signatureSize)); err != nil {
			return 0, "", nil, err
		}
	}
	hash, _, err := hashFile(path)
	if err != nil {
		return 0, "", nil, err
	}
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.path
	}
	return version, hash, paths, nil
}

func parseVPKTree(tree []byte, fileDataSize uint64) ([]vpkEntry, error) {
	position := 0
	entries := make([]vpkEntry, 0)
	seen := make(map[string]bool)
	for {
		extension, err := readVPKString(tree, &position)
		if err != nil {
			return nil, err
		}
		if extension == "" {
			break
		}
		if !validVPKLeaf(extension) {
			return nil, errors.New("VPK contains an invalid extension component")
		}
		for {
			directory, err := readVPKString(tree, &position)
			if err != nil {
				return nil, err
			}
			if directory == "" {
				break
			}
			if directory == " " {
				directory = ""
			}
			if !validVPKDirectory(directory) {
				return nil, errors.New("VPK contains an unsafe directory path")
			}
			for {
				filename, err := readVPKString(tree, &position)
				if err != nil {
					return nil, err
				}
				if filename == "" {
					break
				}
				if !validVPKLeaf(filename) || len(tree)-position < 18 {
					return nil, errors.New("VPK contains an invalid or truncated directory entry")
				}
				header := tree[position : position+18]
				position += 18
				preloadBytes := int(binary.LittleEndian.Uint16(header[4:6]))
				archiveIndex := binary.LittleEndian.Uint16(header[6:8])
				offset := binary.LittleEndian.Uint32(header[8:12])
				length := binary.LittleEndian.Uint32(header[12:16])
				if binary.LittleEndian.Uint16(header[16:18]) != 0xffff {
					return nil, errors.New("VPK directory entry terminator is invalid")
				}
				if archiveIndex != vpkEmbeddedArchive {
					return nil, errors.New("multi-file VPK archives are not supported by single-file installation")
				}
				if preloadBytes > len(tree)-position || uint64(offset)+uint64(length) > fileDataSize {
					return nil, errors.New("VPK entry data exceeds section bounds")
				}
				fullName := filename
				if extension != " " {
					fullName += "." + extension
				}
				fullPath := pathpkg.Join(directory, fullName)
				key := strings.ToLower(fullPath)
				if seen[key] {
					return nil, fmt.Errorf("VPK contains duplicate path %q", fullPath)
				}
				seen[key] = true
				preload := append([]byte(nil), tree[position:position+preloadBytes]...)
				position += preloadBytes
				entries = append(entries, vpkEntry{path: fullPath, crc: binary.LittleEndian.Uint32(header[:4]), preload: preload, offset: offset, length: length})
				if len(entries) > maxVPKEntries {
					return nil, errors.New("VPK file count exceeds limit")
				}
			}
		}
	}
	if position != len(tree) || len(entries) == 0 {
		return nil, errors.New("VPK directory tree has trailing data or no files")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].offset < entries[j].offset })
	for index := 1; index < len(entries); index++ {
		previousEnd := uint64(entries[index-1].offset) + uint64(entries[index-1].length)
		if entries[index-1].length > 0 && entries[index].length > 0 && previousEnd > uint64(entries[index].offset) {
			return nil, errors.New("VPK embedded data ranges overlap")
		}
	}
	return entries, nil
}

func readVPKString(tree []byte, position *int) (string, error) {
	if *position >= len(tree) {
		return "", errors.New("VPK directory tree is missing a terminator")
	}
	end := bytes.IndexByte(tree[*position:], 0)
	if end < 0 || end > 4096 {
		return "", errors.New("VPK directory component is unterminated or too long")
	}
	value := tree[*position : *position+end]
	*position += end + 1
	if !utf8.Valid(value) {
		return "", errors.New("VPK directory component is not UTF-8")
	}
	return string(value), nil
}

func validVPKLeaf(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 && !strings.ContainsAny(value, "/\\:\x00")
}

func validVPKDirectory(value string) bool {
	if value == "" {
		return true
	}
	value = strings.ReplaceAll(value, "\\", "/")
	cleaned := pathpkg.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../") && !strings.HasPrefix(cleaned, "/") && !strings.Contains(cleaned, ":")
}

func verifyVPKEntryData(file *os.File, dataStart int64, entries []vpkEntry) error {
	for _, entry := range entries {
		hash := crc32.NewIEEE()
		_, _ = hash.Write(entry.preload)
		if entry.length > 0 {
			reader := io.NewSectionReader(file, dataStart+int64(entry.offset), int64(entry.length))
			if _, err := io.Copy(hash, reader); err != nil {
				return err
			}
		}
		if hash.Sum32() != entry.crc {
			return fmt.Errorf("VPK checksum mismatch for %q", entry.path)
		}
	}
	return nil
}

func validateVPKSignatureSection(file *os.File, offset, size int64) error {
	if size < 8 {
		return errors.New("VPK signature section is truncated")
	}
	section := io.NewSectionReader(file, offset, size)
	var field [4]byte
	if _, err := io.ReadFull(section, field[:]); err != nil {
		return errors.New("VPK public key length is truncated")
	}
	publicKeySize := int64(binary.LittleEndian.Uint32(field[:]))
	if publicKeySize <= 0 || publicKeySize > size-8 {
		return errors.New("VPK public key length is invalid")
	}
	if _, err := io.CopyN(io.Discard, section, publicKeySize); err != nil {
		return errors.New("VPK public key is truncated")
	}
	if _, err := io.ReadFull(section, field[:]); err != nil {
		return errors.New("VPK signature length is truncated")
	}
	signatureLength := int64(binary.LittleEndian.Uint32(field[:]))
	if signatureLength <= 0 || 8+publicKeySize+signatureLength != size {
		return errors.New("VPK signature length is invalid")
	}
	return nil
}
