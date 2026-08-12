package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const mapVPKPartLimit = int64(1536 << 20)

type mapVPKSource struct {
	path         string
	directory    string
	chunkPrefix  string
	dataStart    int64
	fileDataSize int64
	entries      []mapVPKEntry
	chunkSizes   map[uint16]int64
}

type mapVPKEntry struct {
	path         string
	crc          uint32
	preload      []byte
	archiveIndex uint16
	offset       uint32
	length       uint32
	source       *mapVPKSource
	size         int64
}

type mapPreparedEntry struct {
	entry mapVPKEntry
	crc   uint32
	size  int64
}

type mapVPKBuildResult struct {
	Outputs         []string
	PosterOutput    string
	Artifacts       []mapLocalArtifact
	BSPs            []mapLocalBSP
	Modes           []mapLocalMode
	Warnings        []mapLocalWarning
	Name            string
	Author          string
	Version         string
	Description     string
	Poster          mapLocalPoster
	AggregateSHA256 string
	TotalSize       int64
	RemovedFiles    int
	RemovedBytes    int64
}

func buildMapVPKs(ctx context.Context, stagingRoot string, inputs []mapVPKInput, alreadyExpanded int64, taskName, sourceName string) (mapVPKBuildResult, error) {
	if len(inputs) == 0 {
		return mapVPKBuildResult{}, errors.New("map import did not contain a VPK")
	}
	winners := make(map[string]mapVPKEntry)
	var virtualBytes int64
	for _, input := range inputs {
		source, err := openMapVPK(input.Path)
		if err != nil {
			return mapVPKBuildResult{}, fmt.Errorf("validate VPK %s: %w", input.SortKey, err)
		}
		for index := range source.entries {
			entry := source.entries[index]
			entry.source = source
			if virtualBytes > maxMapExpandedBytes-entry.size || alreadyExpanded > maxMapExpandedBytes-(virtualBytes+entry.size) {
				return mapVPKBuildResult{}, errors.New("archive and VPK virtual content exceed the 40 GiB limit")
			}
			virtualBytes += entry.size
			winners[strings.ToLower(entry.path)] = entry
		}
	}

	paths := make([]string, 0, len(winners))
	for key := range winners {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	prepared := make([]mapPreparedEntry, 0, len(paths))
	result := mapVPKBuildResult{}
	var addonInfo []byte
	for _, key := range paths {
		entry := winners[key]
		extension := strings.ToLower(pathpkg.Ext(entry.path))
		if extension == "" || extension == ".vtf" || extension == ".mp3" || extension == ".wav" || extension == ".vmf" || extension == ".vmx" {
			result.RemovedFiles++
			result.RemovedBytes += entry.size
			continue
		}
		crc, preview, dictionaryPresent, err := inspectMapVPKEntry(ctx, entry, strings.EqualFold(entry.path, "addoninfo.txt"), extension == ".bsp")
		if err != nil {
			return mapVPKBuildResult{}, err
		}
		if preview != nil {
			addonInfo = preview
		}
		if extension == ".bsp" && strings.HasPrefix(strings.ToLower(entry.path), "maps/") && strings.Count(entry.path, "/") == 1 {
			startName := strings.TrimSuffix(pathpkg.Base(entry.path), pathpkg.Ext(entry.path))
			bsp := mapLocalBSP{VirtualPath: entry.path, DictionaryPresent: dictionaryPresent}
			if validSourceMapName(startName) {
				bsp.StartName = startName
			} else {
				result.Warnings = append(result.Warnings, mapLocalWarning{Code: "unsafe_bsp_name", Message: "BSP name is not available as a start-map shortcut", VirtualPath: entry.path})
			}
			if !dictionaryPresent {
				result.Warnings = append(result.Warnings, mapLocalWarning{Code: "missing_stringtable_dictionary", Message: "BSP does not contain stringtable_dictionary.dct", VirtualPath: entry.path})
			}
			result.BSPs = append(result.BSPs, bsp)
		}
		prepared = append(prepared, mapPreparedEntry{entry: entry, crc: crc, size: entry.size})
	}
	if len(result.BSPs) == 0 {
		return mapVPKBuildResult{}, errors.New("merged VPK content does not contain maps/*.bsp")
	}
	if len(prepared) == 0 {
		return mapVPKBuildResult{}, errors.New("map cleanup removed every VPK entry")
	}
	mission, poster, posterOutput, missionWarnings := inspectMapMissions(ctx, winners, result.BSPs, stagingRoot)
	result.Warnings = append(result.Warnings, missionWarnings...)
	result.Author, result.Version, result.Description = mission.Author, mission.Version, mission.Description
	result.Modes, result.Poster, result.PosterOutput = mission.Modes, poster, posterOutput
	if mission.Name != "" {
		result.Name = mission.Name
	} else {
		result.Name = mapDisplayName(addonInfo, result.BSPs, taskName, sourceName)
	}

	groups, err := partitionMapVPKEntries(prepared)
	if err != nil {
		return mapVPKBuildResult{}, err
	}
	// Each generated part temporarily coexists with the published object copy
	// until the whole staging tree is removed. Reserve that peak, not merely the
	// final object-library size, so imports cannot consume the final 5 GiB.
	var generatedBytes int64
	for _, group := range groups {
		tree, treeErr := buildMapVPKTree(group)
		if treeErr != nil {
			return mapVPKBuildResult{}, treeErr
		}
		partBytes := int64(12 + len(tree))
		for _, entry := range group {
			if partBytes > mapVPKPartLimit-entry.size {
				return mapVPKBuildResult{}, errors.New("generated VPK part exceeds 1.5 GiB")
			}
			partBytes += entry.size
		}
		if generatedBytes > maxMapExpandedBytes-partBytes {
			return mapVPKBuildResult{}, errors.New("generated map output exceeds the expanded size limit")
		}
		generatedBytes += partBytes
	}
	if err := ensureMapDiskReserve(stagingRoot, generatedBytes*2); err != nil {
		return mapVPKBuildResult{}, err
	}
	outputRoot, err := ResolveWithin(stagingRoot, "outputs")
	if err != nil {
		return mapVPKBuildResult{}, err
	}
	if err := os.MkdirAll(outputRoot, 0o700); err != nil {
		return mapVPKBuildResult{}, err
	}
	aggregate := sha256.New()
	for index, group := range groups {
		output, err := ResolveWithin(outputRoot, fmt.Sprintf("part-%02d.vpk", index))
		if err != nil {
			return mapVPKBuildResult{}, err
		}
		if err := writeDeterministicMapVPK(ctx, output, group); err != nil {
			return mapVPKBuildResult{}, err
		}
		digest, size, err := hashFile(output)
		if err != nil {
			return mapVPKBuildResult{}, err
		}
		if size > mapVPKPartLimit {
			return mapVPKBuildResult{}, errors.New("generated VPK part exceeds 1.5 GiB")
		}
		decoded, _ := hex.DecodeString(digest)
		_, _ = aggregate.Write(decoded)
		var sizeBuffer [8]byte
		binary.LittleEndian.PutUint64(sizeBuffer[:], uint64(size))
		_, _ = aggregate.Write(sizeBuffer[:])
		result.Outputs = append(result.Outputs, output)
		result.Artifacts = append(result.Artifacts, mapLocalArtifact{Ordinal: index, SHA256: digest, Size: size})
		result.TotalSize += size
	}
	result.AggregateSHA256 = hex.EncodeToString(aggregate.Sum(nil))
	return result, nil
}

func openMapVPK(path string) (*mapVPKSource, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil || !linkInfo.Mode().IsRegular() || isReparseOrSymlink(linkInfo) {
		return nil, errors.New("VPK input must be a regular no-follow file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return nil, errors.New("VPK input must be a regular no-follow file")
	}
	var header [28]byte
	if _, err := io.ReadFull(file, header[:12]); err != nil || binary.LittleEndian.Uint32(header[:4]) != vpkSignature {
		return nil, errors.New("invalid or truncated VPK header")
	}
	version := binary.LittleEndian.Uint32(header[4:8])
	treeSize := binary.LittleEndian.Uint32(header[8:12])
	if (version != 1 && version != 2) || treeSize == 0 || treeSize > maxVPKTreeSize {
		return nil, errors.New("unsupported VPK version or invalid directory tree size")
	}
	headerSize := int64(12)
	fileDataSize := info.Size() - headerSize - int64(treeSize)
	if version == 2 {
		if _, err := io.ReadFull(file, header[12:28]); err != nil {
			return nil, errors.New("VPK v2 header is truncated")
		}
		headerSize = 28
		fileDataSize = int64(binary.LittleEndian.Uint32(header[12:16]))
		sections := headerSize + int64(treeSize) + fileDataSize + int64(binary.LittleEndian.Uint32(header[16:20])) +
			int64(binary.LittleEndian.Uint32(header[20:24])) + int64(binary.LittleEndian.Uint32(header[24:28]))
		if sections != info.Size() {
			return nil, errors.New("VPK v2 section sizes are inconsistent")
		}
	}
	if fileDataSize < 0 || headerSize+int64(treeSize) > info.Size() {
		return nil, errors.New("VPK directory tree exceeds file bounds")
	}
	tree := make([]byte, treeSize)
	if _, err := io.ReadFull(file, tree); err != nil {
		return nil, errors.New("VPK directory tree is truncated")
	}
	source := &mapVPKSource{path: path, directory: filepath.Dir(path), dataStart: headerSize + int64(treeSize), fileDataSize: fileDataSize, chunkSizes: make(map[uint16]int64)}
	base := filepath.Base(path)
	if strings.HasSuffix(strings.ToLower(base), "_dir.vpk") {
		source.chunkPrefix = base[:len(base)-len("_dir.vpk")]
	} else {
		source.chunkPrefix = strings.TrimSuffix(base, filepath.Ext(base))
	}
	entries, err := parseMapVPKTree(tree, source)
	if err != nil {
		return nil, err
	}
	source.entries = entries
	return source, nil
}

func parseMapVPKTree(tree []byte, source *mapVPKSource) ([]mapVPKEntry, error) {
	position := 0
	entries := make([]mapVPKEntry, 0)
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
			return nil, errors.New("VPK contains an invalid extension")
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
			if !validMapVirtualDirectory(directory) {
				return nil, errors.New("VPK contains an unsafe virtual directory")
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
				preloadSize := int(binary.LittleEndian.Uint16(header[4:6]))
				archiveIndex := binary.LittleEndian.Uint16(header[6:8])
				offset := binary.LittleEndian.Uint32(header[8:12])
				length := binary.LittleEndian.Uint32(header[12:16])
				if binary.LittleEndian.Uint16(header[16:18]) != 0xffff || preloadSize > len(tree)-position {
					return nil, errors.New("VPK directory entry is truncated")
				}
				fullName := filename
				if extension != " " {
					fullName += "." + extension
				}
				fullPath := pathpkg.Join(directory, fullName)
				if !validMapVirtualPath(fullPath) {
					return nil, errors.New("VPK contains an unsafe virtual path")
				}
				key := strings.ToLower(fullPath)
				if seen[key] {
					return nil, fmt.Errorf("VPK contains duplicate path %q", fullPath)
				}
				seen[key] = true
				entry := mapVPKEntry{path: fullPath, crc: binary.LittleEndian.Uint32(header[:4]), preload: append([]byte(nil), tree[position:position+preloadSize]...), archiveIndex: archiveIndex, offset: offset, length: length}
				position += preloadSize
				entry.size = int64(len(entry.preload)) + int64(entry.length)
				if entry.size > mapVPKPartLimit-12 {
					return nil, fmt.Errorf("VPK entry %q cannot fit in a 1.5 GiB output part", fullPath)
				}
				if err := validateMapVPKEntryRange(source, entry); err != nil {
					return nil, fmt.Errorf("VPK entry %q: %w", fullPath, err)
				}
				entries = append(entries, entry)
				if len(entries) > maxVPKEntries {
					return nil, errors.New("VPK file count exceeds limit")
				}
			}
		}
	}
	if position != len(tree) || len(entries) == 0 {
		return nil, errors.New("VPK directory tree has trailing data or no files")
	}
	if err := validateMapVPKRanges(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func validateMapVPKEntryRange(source *mapVPKSource, entry mapVPKEntry) error {
	end := uint64(entry.offset) + uint64(entry.length)
	if entry.archiveIndex == vpkEmbeddedArchive {
		if end > uint64(source.fileDataSize) {
			return errors.New("embedded data exceeds file bounds")
		}
		return nil
	}
	if entry.archiveIndex > 999 {
		return errors.New("external archive index exceeds supported range")
	}
	if _, exists := source.chunkSizes[entry.archiveIndex]; !exists {
		name := fmt.Sprintf("%s_%03d.vpk", source.chunkPrefix, entry.archiveIndex)
		chunk, err := openManagedFileNoFollow(source.directory, name)
		if err != nil {
			return fmt.Errorf("open external VPK data block %s: %w", name, err)
		}
		info, statErr := chunk.Stat()
		closeErr := chunk.Close()
		if statErr != nil || !info.Mode().IsRegular() || closeErr != nil {
			return errors.New("external VPK data block is invalid")
		}
		source.chunkSizes[entry.archiveIndex] = info.Size()
	}
	if end > uint64(source.chunkSizes[entry.archiveIndex]) {
		return errors.New("external data exceeds block bounds")
	}
	return nil
}

func validateMapVPKRanges(entries []mapVPKEntry) error {
	ordered := append([]mapVPKEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].archiveIndex != ordered[j].archiveIndex {
			return ordered[i].archiveIndex < ordered[j].archiveIndex
		}
		return ordered[i].offset < ordered[j].offset
	})
	for index := 1; index < len(ordered); index++ {
		previous, current := ordered[index-1], ordered[index]
		if previous.archiveIndex == current.archiveIndex && previous.length > 0 && current.length > 0 && uint64(previous.offset)+uint64(previous.length) > uint64(current.offset) {
			return errors.New("VPK data ranges overlap")
		}
	}
	return nil
}

func validMapVirtualDirectory(directory string) bool {
	return validVPKDirectory(directory)
}

func validMapVirtualPath(value string) bool {
	return value == pathpkg.Clean(value) && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../") && !strings.Contains(value, "\\")
}

func (entry mapVPKEntry) open() (io.ReadCloser, error) {
	var file *os.File
	var err error
	var start int64
	if entry.archiveIndex == vpkEmbeddedArchive {
		file, err = os.Open(entry.source.path)
		start = entry.source.dataStart + int64(entry.offset)
	} else {
		name := fmt.Sprintf("%s_%03d.vpk", entry.source.chunkPrefix, entry.archiveIndex)
		file, err = openManagedFileNoFollow(entry.source.directory, name)
		start = int64(entry.offset)
	}
	if err != nil {
		return nil, err
	}
	reader := io.MultiReader(bytes.NewReader(entry.preload), io.NewSectionReader(file, start, int64(entry.length)))
	return &mapEntryReader{Reader: reader, file: file}, nil
}

type mapEntryReader struct {
	io.Reader
	file *os.File
}

func (r *mapEntryReader) Close() error { return r.file.Close() }

func inspectMapVPKEntry(ctx context.Context, entry mapVPKEntry, capture, checkDictionary bool) (uint32, []byte, bool, error) {
	reader, err := entry.open()
	if err != nil {
		return 0, nil, false, err
	}
	defer reader.Close()
	hash := crc32.NewIEEE()
	var preview bytes.Buffer
	var writer io.Writer = hash
	if capture && entry.size <= 1<<20 {
		writer = io.MultiWriter(hash, &preview)
	}
	needle := []byte("stringtable_dictionary.dct")
	found := false
	tail := make([]byte, 0, len(needle)-1)
	buffer := make([]byte, 128<<10)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, false, err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			read += int64(count)
			_, _ = writer.Write(buffer[:count])
			if checkDictionary && !found {
				window := append(tail, buffer[:count]...)
				found = bytes.Contains(window, needle)
				if len(window) >= len(needle)-1 {
					tail = append(tail[:0], window[len(window)-(len(needle)-1):]...)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, nil, false, readErr
		}
	}
	if read != entry.size || hash.Sum32() != entry.crc {
		return 0, nil, false, fmt.Errorf("VPK checksum mismatch for %q", entry.path)
	}
	return hash.Sum32(), preview.Bytes(), found, nil
}

func validSourceMapName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func mapDisplayName(addonInfo []byte, bsps []mapLocalBSP, taskName, sourceName string) string {
	if title := addonTitle(addonInfo); title != "" {
		return title
	}
	if title := safeMapDisplayName(taskName); title != "" {
		return title
	}
	if len(bsps) > 0 {
		name := strings.TrimSuffix(pathpkg.Base(bsps[0].VirtualPath), pathpkg.Ext(bsps[0].VirtualPath))
		if name != "" {
			return name
		}
	}
	name := safeMapDisplayName(strings.TrimSuffix(filepath.Base(sourceName), filepath.Ext(sourceName)))
	if name == "" {
		return "未命名地图"
	}
	return name
}

func safeMapDisplayName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return truncateUTF8(value, 200)
}

func addonTitle(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if strings.HasPrefix(line, "//") || !strings.Contains(strings.ToLower(line), "addontitle") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(strings.Trim(fields[0], "\""), "addontitle") {
			continue
		}
		value := strings.Trim(strings.Join(fields[1:], " "), " \t\r\n\"")
		if value != "" {
			return truncateUTF8(value, 200)
		}
	}
	return ""
}

func truncateUTF8(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func partitionMapVPKEntries(entries []mapPreparedEntry) ([][]mapPreparedEntry, error) {
	groups := make([][]mapPreparedEntry, 0)
	current := make([]mapPreparedEntry, 0)
	var dataSize, estimatedTreeSize int64
	for _, entry := range entries {
		if entry.size > mapVPKPartLimit-12 {
			return nil, fmt.Errorf("VPK entry %q cannot fit in a 1.5 GiB part", entry.entry.path)
		}
		entryTreeEstimate := int64(len(entry.entry.path) + 40)
		if len(current) > 0 && 12+estimatedTreeSize+entryTreeEstimate+dataSize+entry.size > mapVPKPartLimit {
			groups = append(groups, current)
			current = []mapPreparedEntry{entry}
			dataSize = entry.size
			estimatedTreeSize = entryTreeEstimate
			continue
		}
		current = append(current, entry)
		dataSize += entry.size
		estimatedTreeSize += entryTreeEstimate
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	if len(groups) > 64 {
		return nil, errors.New("map requires more than 64 VPK parts")
	}
	return groups, nil
}

type mapTreeEntry struct {
	prepared  mapPreparedEntry
	extension string
	directory string
	filename  string
	offset    uint32
}

func buildMapVPKTree(entries []mapPreparedEntry) ([]byte, error) {
	treeEntries := make([]mapTreeEntry, len(entries))
	var offset uint64
	for index, prepared := range entries {
		directory, base := pathpkg.Split(prepared.entry.path)
		directory = strings.TrimSuffix(directory, "/")
		extension := strings.TrimPrefix(pathpkg.Ext(base), ".")
		filename := strings.TrimSuffix(base, pathpkg.Ext(base))
		if extension == "" {
			extension = " "
		}
		if directory == "" {
			directory = " "
		}
		if offset > uint64(^uint32(0)) || prepared.size > int64(^uint32(0)) {
			return nil, errors.New("VPK v1 offset exceeds format limits")
		}
		treeEntries[index] = mapTreeEntry{prepared: prepared, extension: extension, directory: directory, filename: filename, offset: uint32(offset)}
		offset += uint64(prepared.size)
	}
	sort.Slice(treeEntries, func(i, j int) bool {
		left := strings.ToLower(treeEntries[i].extension + "\x00" + treeEntries[i].directory + "\x00" + treeEntries[i].filename)
		right := strings.ToLower(treeEntries[j].extension + "\x00" + treeEntries[j].directory + "\x00" + treeEntries[j].filename)
		if left == right {
			return treeEntries[i].prepared.entry.path < treeEntries[j].prepared.entry.path
		}
		return left < right
	})
	var tree bytes.Buffer
	for extensionIndex := 0; extensionIndex < len(treeEntries); {
		extension := treeEntries[extensionIndex].extension
		writeVPKCString(&tree, extension)
		for directoryIndex := extensionIndex; directoryIndex < len(treeEntries) && strings.EqualFold(treeEntries[directoryIndex].extension, extension); {
			directory := treeEntries[directoryIndex].directory
			writeVPKCString(&tree, directory)
			filenameIndex := directoryIndex
			for ; filenameIndex < len(treeEntries) && strings.EqualFold(treeEntries[filenameIndex].extension, extension) && strings.EqualFold(treeEntries[filenameIndex].directory, directory); filenameIndex++ {
				item := treeEntries[filenameIndex]
				writeVPKCString(&tree, item.filename)
				var header [18]byte
				binary.LittleEndian.PutUint32(header[:4], item.prepared.crc)
				binary.LittleEndian.PutUint16(header[6:8], vpkEmbeddedArchive)
				binary.LittleEndian.PutUint32(header[8:12], item.offset)
				binary.LittleEndian.PutUint32(header[12:16], uint32(item.prepared.size))
				binary.LittleEndian.PutUint16(header[16:18], 0xffff)
				_, _ = tree.Write(header[:])
			}
			_ = tree.WriteByte(0)
			directoryIndex = filenameIndex
		}
		_ = tree.WriteByte(0)
		for extensionIndex < len(treeEntries) && strings.EqualFold(treeEntries[extensionIndex].extension, extension) {
			extensionIndex++
		}
	}
	_ = tree.WriteByte(0)
	if tree.Len() > int(maxVPKTreeSize) {
		return nil, errors.New("generated VPK directory tree exceeds limit")
	}
	return tree.Bytes(), nil
}

func writeVPKCString(buffer *bytes.Buffer, value string) {
	_, _ = buffer.WriteString(value)
	_ = buffer.WriteByte(0)
}

func writeDeterministicMapVPK(ctx context.Context, output string, entries []mapPreparedEntry) error {
	tree, err := buildMapVPKTree(entries)
	if err != nil {
		return err
	}
	temporary := output + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	var header [12]byte
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	if _, err := file.Write(header[:]); err != nil {
		return err
	}
	if _, err := file.Write(tree); err != nil {
		return err
	}
	for _, prepared := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader, err := prepared.entry.open()
		if err != nil {
			return err
		}
		copied, copyErr := io.CopyN(file, &mapContextReader{ctx: ctx, reader: reader}, prepared.size)
		closeErr := reader.Close()
		if copyErr != nil || copied != prepared.size {
			return fmt.Errorf("write VPK entry %q: %w", prepared.entry.path, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, output); err != nil {
		return err
	}
	committed = true
	return nil
}
