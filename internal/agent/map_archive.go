package agent

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
	"github.com/shirou/gopsutil/v4/disk"
)

const (
	maxMapArchiveDepth     = 3
	maxMapArchiveEntries   = 100000
	maxMapArchiveEntrySize = int64(20 << 30)
	maxMapExpandedBytes    = int64(40 << 30)
	maxMapCompressionRatio = int64(100)
	mapDiskFreeReserve     = uint64(5 << 30)
	maxMapQQFlashDownload  = int64(20 << 30)
	maxMapQQFlashFiles     = 100000
)

type mapArchiveState struct {
	Entries         int
	Expanded        int64
	Ignored         int
	Root            string
	MaxEntries      int
	MaxEntrySize    int64
	MaxExpanded     int64
	DiskFreeReserve uint64
	Subject         string
	SelectiveInputs bool
	PathEncoding    string
	PathDecodes     mapPathDecodeStats
}

type mapVPKInput struct {
	Path    string
	SortKey string
}

type mapInputDiscovery struct {
	Inputs      []mapVPKInput
	Ignored     int
	Expanded    int64
	PathDecodes mapPathDecodeStats
}

type mapFileKind int

const (
	mapFileUnknown mapFileKind = iota
	mapFileVPK
	mapFileZIP
	mapFile7Z
	mapFileRAR
)

func detectMapFileKind(path string) (mapFileKind, error) {
	file, err := os.Open(path)
	if err != nil {
		return mapFileUnknown, err
	}
	defer file.Close()
	var signature [8]byte
	count, err := io.ReadFull(file, signature[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return mapFileUnknown, err
	}
	return detectMapFileKindFromSignature(signature[:count]), nil
}

func detectMapFileKindFromSignature(signature []byte) mapFileKind {
	count := len(signature)
	if count >= 4 && uint32(signature[0])|uint32(signature[1])<<8|uint32(signature[2])<<16|uint32(signature[3])<<24 == vpkSignature {
		return mapFileVPK
	}
	if count >= 4 && signature[0] == 'P' && signature[1] == 'K' &&
		((signature[2] == 3 && signature[3] == 4) || (signature[2] == 5 && signature[3] == 6) || (signature[2] == 7 && signature[3] == 8)) {
		return mapFileZIP
	}
	if count >= 6 && string(signature[:6]) == "7z\xbc\xaf'\x1c" {
		return mapFile7Z
	}
	if count >= 7 && string(signature[:7]) == "Rar!\x1a\x07\x00" {
		return mapFileRAR
	}
	if count >= 8 && string(signature[:8]) == "Rar!\x1a\x07\x01\x00" {
		return mapFileRAR
	}
	return mapFileUnknown
}

func discoverMapInputs(ctx context.Context, stagingRoot string, topLevel []qqFlashListedFile, initialEntries int, deviceID string,
	client *qqFlashClient, pathEncoding string, progress ProgressFunc) (mapInputDiscovery, error) {
	pathEncoding, err := normalizeMapPathEncoding(pathEncoding)
	if err != nil {
		return mapInputDiscovery{}, err
	}
	var downloadBytes int64
	for _, listed := range topLevel {
		if listed.File.Size < 0 || downloadBytes > maxMapQQFlashDownload-listed.File.Size {
			return mapInputDiscovery{}, errors.New("QQ flash share exceeds the map download limit")
		}
		downloadBytes += listed.File.Size
	}
	usage, err := disk.Usage(stagingRoot)
	if err != nil {
		return mapInputDiscovery{}, fmt.Errorf("read map staging free space: %w", err)
	}
	if uint64(downloadBytes) > usage.Free || usage.Free-uint64(downloadBytes) < mapDiskFreeReserve {
		return mapInputDiscovery{}, errors.New("map download would consume the reserved free disk space")
	}
	state := &mapArchiveState{Root: stagingRoot, Entries: initialEntries, SelectiveInputs: true, PathEncoding: pathEncoding}
	inputs := make([]mapVPKInput, 0)
	for index, listed := range topLevel {
		if ctx.Err() != nil {
			return mapInputDiscovery{}, ctx.Err()
		}
		relative := filepath.ToSlash(filepath.Join("downloads", filepath.FromSlash(listed.Relative)))
		progress(8+index*25/max(1, len(topLevel)), "downloading QQ flash map files")
		if err := client.downloadFile(ctx, stagingRoot, relative, listed.File, deviceID, nil); err != nil {
			return mapInputDiscovery{}, &mapImportInputError{Stage: mapImportStageDownload, Source: listed.Relative, Err: err}
		}
		path, err := ResolveWithin(stagingRoot, filepath.FromSlash(relative))
		if err != nil {
			return mapInputDiscovery{}, err
		}
		found, _, err := discoverMapFile(ctx, stagingRoot, path, listed.Relative, 0, state)
		if err != nil {
			return mapInputDiscovery{}, &mapImportInputError{Stage: mapImportStageArchive, Source: listed.Relative, Err: err}
		}
		inputs = append(inputs, found...)
	}
	sortMapInputs(inputs)
	return mapInputDiscovery{Inputs: inputs, Ignored: state.Ignored, Expanded: state.Expanded, PathDecodes: state.PathDecodes}, nil
}

func discoverMapFile(ctx context.Context, stagingRoot, filePath, sortKey string, depth int, state *mapArchiveState) ([]mapVPKInput, bool, error) {
	kind, err := detectMapFileKind(filePath)
	if err != nil {
		return nil, false, err
	}
	switch kind {
	case mapFileVPK:
		return []mapVPKInput{{Path: filePath, SortKey: filepath.ToSlash(sortKey)}}, true, nil
	case mapFileZIP, mapFile7Z, mapFileRAR:
		if depth >= maxMapArchiveDepth {
			return nil, true, errors.New("nested archive depth exceeds three levels")
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", depth, filepath.ToSlash(sortKey))))
		relativeDestination := filepath.ToSlash(filepath.Join("expanded", hex.EncodeToString(digest[:8])))
		destination, err := ResolveWithin(stagingRoot, filepath.FromSlash(relativeDestination))
		if err != nil {
			return nil, true, err
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return nil, true, err
		}
		if err := rejectLinkedAncestors(destination); err != nil {
			return nil, true, err
		}
		files, err := extractMapArchive(ctx, filePath, destination, kind, state)
		if err != nil {
			return nil, true, err
		}
		inputs := make([]mapVPKInput, 0)
		for _, extracted := range files {
			childKey := filepath.ToSlash(sortKey) + "!/" + filepath.ToSlash(extracted.Relative)
			found, _, err := discoverMapFile(ctx, stagingRoot, extracted.Path, childKey, depth+1, state)
			if err != nil {
				return nil, true, err
			}
			inputs = append(inputs, found...)
		}
		return inputs, true, nil
	default:
		if potentialExternalVPKChunk(filePath) {
			return nil, true, nil
		}
		state.Ignored++
		return nil, false, nil
	}
}

func potentialExternalVPKChunk(filePath string) bool {
	base := strings.ToLower(filepath.Base(filePath))
	if !strings.HasSuffix(base, ".vpk") {
		return false
	}
	stem := strings.TrimSuffix(base, ".vpk")
	if len(stem) < 4 || stem[len(stem)-4] != '_' {
		return false
	}
	for _, char := range stem[len(stem)-3:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

type extractedMapFile struct {
	Path     string
	Relative string
}

func extractMapArchive(ctx context.Context, archivePath, destination string, kind mapFileKind, state *mapArchiveState) ([]extractedMapFile, error) {
	switch kind {
	case mapFileZIP:
		return extractMapZIP(ctx, archivePath, destination, state)
	case mapFile7Z:
		return extractMap7Z(ctx, archivePath, destination, state)
	case mapFileRAR:
		return extractMapRAR(ctx, archivePath, destination, state)
	default:
		return nil, errors.New("unsupported map archive")
	}
}

func extractMapZIP(ctx context.Context, archivePath, destination string, state *mapArchiveState) ([]extractedMapFile, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("ZIP archive is not a regular file")
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, err
	}
	files := make([]extractedMapFile, 0)
	seen := make(map[string]bool)
	for _, entry := range reader.File {
		if entry.Flags&1 != 0 {
			return nil, errors.New("encrypted ZIP archives are not supported")
		}
		mode := entry.Mode()
		if mode&fs.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return nil, errors.New("ZIP archive contains a link or special file")
		}
		entryName := entry.Name
		if state.SelectiveInputs {
			entryName, err = decodeMapZIPPath(entry.Name, entry.NonUTF8, state.PathEncoding, &state.PathDecodes)
			if err != nil {
				return nil, fmt.Errorf("decode ZIP path: %w", err)
			}
		}
		relative, err := validateMapArchivePathForState(entryName, state)
		if err != nil {
			return nil, err
		}
		if !state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			return nil, errors.New("ZIP archive contains colliding paths")
		}
		if err := state.countEntry(); err != nil {
			return nil, err
		}
		if mode.IsDir() {
			continue
		}
		size := int64(entry.UncompressedSize64)
		compressed := int64(entry.CompressedSize64)
		if state.SelectiveInputs {
			if err := state.validateEntryBounds(size, compressed); err != nil {
				return nil, err
			}
		}
		source, err := entry.Open()
		if err != nil {
			return nil, err
		}
		selected, keep, err := selectMapArchiveInput(state, destination, relative, source, size, compressed)
		if err != nil {
			_ = source.Close()
			return nil, err
		}
		if !keep {
			_ = source.Close()
			continue
		}
		if state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			_ = source.Close()
			return nil, errors.New("ZIP archive contains colliding selected input paths")
		}
		err = writeMapArchiveFile(ctx, destination, relative, selected, size)
		closeErr := source.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		path, _ := ResolveWithin(destination, filepath.FromSlash(relative))
		files = append(files, extractedMapFile{Path: path, Relative: relative})
	}
	return files, nil
}

func extractMap7Z(ctx context.Context, archivePath, destination string, state *mapArchiveState) ([]extractedMapFile, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("7Z archive is not a regular file")
	}
	reader, err := sevenzip.NewReader(file, info.Size())
	if err != nil {
		var readErr *sevenzip.ReadError
		if errors.As(err, &readErr) && readErr.Encrypted {
			return nil, errors.New("encrypted 7Z archives are not supported")
		}
		return nil, err
	}
	var declared int64
	for _, entry := range reader.File {
		if entry.UncompressedSize > uint64(state.maxEntrySize()) || declared > state.maxExpandedBytes()-int64(entry.UncompressedSize) {
			return nil, errors.New("7Z archive exceeds the expanded size limit")
		}
		declared += int64(entry.UncompressedSize)
	}
	if info.Size() <= 0 || declared > info.Size()*maxMapCompressionRatio {
		return nil, errors.New("7Z archive exceeds the compression ratio limit")
	}
	files := make([]extractedMapFile, 0)
	seen := make(map[string]bool)
	for _, entry := range reader.File {
		mode := entry.Mode()
		if mode&fs.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return nil, errors.New("7Z archive contains a link or special file")
		}
		relative, err := validateMapArchivePathForState(entry.Name, state)
		if err != nil {
			return nil, err
		}
		if !state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			return nil, errors.New("7Z archive contains colliding paths")
		}
		if err := state.countEntry(); err != nil {
			return nil, err
		}
		if mode.IsDir() {
			continue
		}
		size := int64(entry.UncompressedSize)
		source, err := entry.Open()
		if err != nil {
			var readErr *sevenzip.ReadError
			if errors.As(err, &readErr) && readErr.Encrypted {
				return nil, errors.New("encrypted 7Z archives are not supported")
			}
			return nil, err
		}
		selected, keep, err := selectMapArchiveInput(state, destination, relative, source, size, -1)
		if err != nil {
			_ = source.Close()
			return nil, err
		}
		if !keep {
			_ = source.Close()
			continue
		}
		if state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			_ = source.Close()
			return nil, errors.New("7Z archive contains colliding selected input paths")
		}
		err = writeMapArchiveFile(ctx, destination, relative, selected, size)
		closeErr := source.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		path, _ := ResolveWithin(destination, filepath.FromSlash(relative))
		files = append(files, extractedMapFile{Path: path, Relative: relative})
	}
	return files, nil
}

func extractMapRAR(ctx context.Context, archivePath, destination string, state *mapArchiveState) ([]extractedMapFile, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader, err := rardecode.NewReader(bufio.NewReader(file), rardecode.MaxDictionarySize(512<<20))
	if err != nil {
		return nil, err
	}
	files := make([]extractedMapFile, 0)
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, rardecode.ErrMultiVolume) {
			return nil, errors.New("multi-volume RAR archives are not supported")
		}
		if err != nil {
			return nil, err
		}
		if header.Encrypted || header.HeaderEncrypted {
			return nil, errors.New("encrypted RAR archives are not supported")
		}
		mode := header.Mode()
		if mode&fs.ModeSymlink != 0 || (!header.IsDir && !mode.IsRegular()) {
			return nil, errors.New("RAR archive contains a link or special file")
		}
		relative, err := validateMapArchivePathForState(header.Name, state)
		if err != nil {
			return nil, err
		}
		if !state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			return nil, errors.New("RAR archive contains colliding paths")
		}
		if err := state.countEntry(); err != nil {
			return nil, err
		}
		if header.IsDir {
			continue
		}
		if header.UnKnownSize {
			return nil, errors.New("RAR entry has an unknown expanded size")
		}
		if state.SelectiveInputs {
			if err := state.validateEntryBounds(header.UnPackedSize, header.PackedSize); err != nil {
				return nil, err
			}
		}
		selected, keep, err := selectMapArchiveInput(state, destination, relative, reader, header.UnPackedSize, header.PackedSize)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		if state.SelectiveInputs && !rememberMapArchivePath(seen, relative) {
			return nil, errors.New("RAR archive contains colliding selected input paths")
		}
		if err := writeMapArchiveFile(ctx, destination, relative, selected, header.UnPackedSize); err != nil {
			return nil, err
		}
		path, _ := ResolveWithin(destination, filepath.FromSlash(relative))
		files = append(files, extractedMapFile{Path: path, Relative: relative})
	}
	return files, nil
}

func rememberMapArchivePath(seen map[string]bool, relative string) bool {
	key := strings.ToLower(relative)
	if seen[key] {
		return false
	}
	seen[key] = true
	return true
}

func validateMapArchivePath(name string) (string, error) {
	if name == "" || len(name) > 4096 || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", errors.New("archive contains an unsafe path")
	}
	cleaned := pathpkg.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != strings.TrimSuffix(name, "/") {
		return "", errors.New("archive contains an unsafe path")
	}
	for _, component := range strings.Split(cleaned, "/") {
		if err := validateQQFlashEntryName(component); err != nil {
			return "", errors.New("archive contains an unsafe path component")
		}
	}
	return cleaned, nil
}

func validateMapArchivePathForState(name string, state *mapArchiveState) (string, error) {
	if state != nil && state.SelectiveInputs && !utf8.ValidString(name) {
		return "", errors.New("archive path is not valid UTF-8 after decoding")
	}
	return validateMapArchivePath(name)
}

func selectMapArchiveInput(state *mapArchiveState, destination, relative string, source io.Reader, size, compressed int64) (io.Reader, bool, error) {
	if !state.SelectiveInputs {
		if err := state.reserveEntry(destination, size, compressed); err != nil {
			return nil, false, err
		}
		return source, true, nil
	}
	var signature [8]byte
	count, err := io.ReadFull(source, signature[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	prefix := signature[:count]
	if detectMapFileKindFromSignature(prefix) == mapFileUnknown && !potentialExternalVPKChunk(relative) {
		state.Ignored++
		return nil, false, nil
	}
	if err := state.reserveEntry(destination, size, compressed); err != nil {
		return nil, false, err
	}
	return io.MultiReader(bytes.NewReader(prefix), source), true, nil
}

func (s *mapArchiveState) countEntry() error {
	s.Entries++
	limit := s.MaxEntries
	if limit <= 0 {
		limit = maxMapArchiveEntries
	}
	if s.Entries > limit {
		return fmt.Errorf("%s archive entry count exceeds limit", s.subject())
	}
	return nil
}

func (s *mapArchiveState) reserveEntry(path string, expanded, compressed int64) error {
	if err := s.validateEntryBounds(expanded, compressed); err != nil {
		return err
	}
	if s.Expanded > s.maxExpandedBytes()-expanded {
		return fmt.Errorf("%s archive expanded size exceeds limit", s.subject())
	}
	reserve := s.DiskFreeReserve
	if reserve == 0 {
		reserve = mapDiskFreeReserve
	}
	if err := ensureArchiveDiskReserve(path, expanded, reserve, s.subject()); err != nil {
		return err
	}
	s.Expanded += expanded
	return nil
}

func (s *mapArchiveState) validateEntryBounds(expanded, compressed int64) error {
	if expanded < 0 || expanded > s.maxEntrySize() {
		return fmt.Errorf("%s archive entry size exceeds limit", s.subject())
	}
	if expanded > 0 && compressed == 0 {
		return fmt.Errorf("%s archive entry exceeds the compression ratio limit", s.subject())
	}
	if compressed > 0 && expanded > compressed*maxMapCompressionRatio {
		return fmt.Errorf("%s archive entry exceeds the compression ratio limit", s.subject())
	}
	return nil
}

func (s *mapArchiveState) maxEntrySize() int64 {
	if s.MaxEntrySize > 0 {
		return s.MaxEntrySize
	}
	return maxMapArchiveEntrySize
}

func (s *mapArchiveState) maxExpandedBytes() int64 {
	if s.MaxExpanded > 0 {
		return s.MaxExpanded
	}
	return maxMapExpandedBytes
}

func (s *mapArchiveState) subject() string {
	if strings.TrimSpace(s.Subject) != "" {
		return strings.TrimSpace(s.Subject)
	}
	return "map"
}

func ensureArchiveDiskReserve(path string, additional int64, reserve uint64, subject string) error {
	if additional < 0 {
		return fmt.Errorf("%s processing requested an invalid disk reservation", subject)
	}
	usage, err := disk.Usage(path)
	if err != nil {
		return fmt.Errorf("read %s staging free space: %w", subject, err)
	}
	if uint64(additional) > usage.Free || usage.Free-uint64(additional) < reserve {
		return fmt.Errorf("%s processing would consume the reserved free disk space", subject)
	}
	return nil
}

func ensureMapDiskReserve(path string, additional int64) error {
	return ensureArchiveDiskReserve(path, additional, mapDiskFreeReserve, "map")
}

func writeMapArchiveFile(ctx context.Context, destination, relative string, source io.Reader, size int64) error {
	reader := &mapContextReader{ctx: ctx, reader: io.LimitReader(source, size+1)}
	_, err := writeFileWithinNoFollow(destination, relative, reader, secureWriteOptions{
		Mode: 0o600, ExpectedSize: size, Replace: false, RejectExistingLink: true,
	})
	return err
}

type mapContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *mapContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func sortMapInputs(inputs []mapVPKInput) {
	sort.Slice(inputs, func(i, j int) bool {
		first, second := strings.ToLower(filepath.ToSlash(inputs[i].SortKey)), strings.ToLower(filepath.ToSlash(inputs[j].SortKey))
		if first == second {
			return inputs[i].SortKey < inputs[j].SortKey
		}
		return first < second
	})
}
