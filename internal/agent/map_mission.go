package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
)

const (
	maxMapMissionBytes = int64(1 << 20)
	maxMapVMTBytes     = int64(256 << 10)
	maxMapPosterVTF    = int64(32 << 20)
	maxMapPosterJPEG   = 1 << 20
	maxMapKVDepth      = 16
	maxMapKVPairs      = 20000
	maxMapLocalization = int64(8 << 20)
)

type mapLocalChapter struct {
	Ordinal     int    `json:"ordinal"`
	BSPName     string `json:"bsp_name"`
	DisplayName string `json:"display_name"`
	Image       string `json:"image,omitempty"`
}

type mapLocalMode struct {
	Name     string            `json:"name"`
	Chapters []mapLocalChapter `json:"chapters"`
}

type mapLocalPoster struct {
	Relative string `json:"relative,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Size     int64  `json:"size,omitempty"`
	MIME     string `json:"mime,omitempty"`
}

type mapMissionMetadata struct {
	Path             string
	Name             string
	Version          string
	Author           string
	Description      string
	PosterReferences []string
	Modes            []mapLocalMode
}

type mapKVPair struct {
	Key      string
	Value    string
	Children []mapKVPair
}

type mapKVParser struct {
	raw   []byte
	pos   int
	pairs int
}

func parseMapKeyValues(raw []byte) ([]mapKVPair, error) {
	decoded, err := decodeMapKVDocument(raw)
	if err != nil {
		return nil, err
	}
	parser := &mapKVParser{raw: decoded}
	pairs, closed, err := parser.parsePairs(0, false)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, errors.New("KeyValues document has an unexpected closing brace")
	}
	if err := parser.skipSpaceAndComments(); err != nil {
		return nil, err
	}
	if parser.pos != len(parser.raw) || len(pairs) == 0 {
		return nil, errors.New("KeyValues document has trailing data or no keys")
	}
	return pairs, nil
}

func decodeMapKVDocument(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > int(maxMapMissionBytes) {
		return nil, errors.New("KeyValues document is empty or too large")
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		raw = raw[3:]
	}
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		littleEndian := raw[0] == 0xff
		raw = raw[2:]
		if len(raw)%2 != 0 {
			return nil, errors.New("UTF-16 KeyValues document has an odd byte length")
		}
		units := make([]uint16, len(raw)/2)
		for index := range units {
			if littleEndian {
				units[index] = binary.LittleEndian.Uint16(raw[index*2 : index*2+2])
			} else {
				units[index] = binary.BigEndian.Uint16(raw[index*2 : index*2+2])
			}
		}
		raw = []byte(string(utf16.Decode(units)))
	}
	if len(raw) == 0 || len(raw) > 2<<20 || !utf8.Valid(raw) {
		return nil, errors.New("KeyValues document encoding or decoded size is invalid")
	}
	return raw, nil
}

func (p *mapKVParser) parsePairs(depth int, expectClose bool) ([]mapKVPair, bool, error) {
	if depth > maxMapKVDepth {
		return nil, false, errors.New("KeyValues nesting exceeds limit")
	}
	result := make([]mapKVPair, 0)
	for {
		token, kind, err := p.nextToken()
		if err != nil {
			return nil, false, err
		}
		if kind == 0 {
			if expectClose {
				return nil, false, errors.New("KeyValues object is missing a closing brace")
			}
			return result, false, nil
		}
		if kind == '}' {
			if !expectClose {
				return nil, true, nil
			}
			return result, true, nil
		}
		if kind == '{' || token == "" {
			return nil, false, errors.New("KeyValues object contains an invalid key")
		}
		value, valueKind, err := p.nextToken()
		if err != nil {
			return nil, false, err
		}
		pair := mapKVPair{Key: token}
		switch valueKind {
		case '{':
			children, closed, err := p.parsePairs(depth+1, true)
			if err != nil {
				return nil, false, err
			}
			if !closed {
				return nil, false, errors.New("KeyValues object is missing a closing brace")
			}
			pair.Children = children
		case 0, '}':
			return nil, false, errors.New("KeyValues key is missing a value")
		default:
			pair.Value = value
		}
		p.pairs++
		if p.pairs > maxMapKVPairs {
			return nil, false, errors.New("KeyValues pair count exceeds limit")
		}
		result = append(result, pair)
	}
}

func (p *mapKVParser) nextToken() (string, byte, error) {
	if err := p.skipSpaceAndComments(); err != nil {
		return "", 0, err
	}
	if p.pos >= len(p.raw) {
		return "", 0, nil
	}
	if p.raw[p.pos] == '{' || p.raw[p.pos] == '}' {
		kind := p.raw[p.pos]
		p.pos++
		return string(kind), kind, nil
	}
	if p.raw[p.pos] == '"' {
		p.pos++
		var value strings.Builder
		for p.pos < len(p.raw) {
			char := p.raw[p.pos]
			p.pos++
			if char == '"' {
				if value.Len() > 4096 {
					return "", 0, errors.New("KeyValues token exceeds limit")
				}
				return value.String(), 's', nil
			}
			if char == '\\' && p.pos < len(p.raw) {
				escaped := p.raw[p.pos]
				p.pos++
				switch escaped {
				case 'n':
					value.WriteByte('\n')
				case 'r':
					value.WriteByte('\r')
				case 't':
					value.WriteByte('\t')
				default:
					value.WriteByte(escaped)
				}
				continue
			}
			if char == 0 {
				return "", 0, errors.New("KeyValues token contains NUL")
			}
			value.WriteByte(char)
		}
		return "", 0, errors.New("KeyValues quoted token is unterminated")
	}
	start := p.pos
	for p.pos < len(p.raw) {
		char := p.raw[p.pos]
		if char == '{' || char == '}' || char == 0 || char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			break
		}
		p.pos++
	}
	if p.pos == start || p.pos-start > 4096 {
		return "", 0, errors.New("KeyValues token is invalid or too long")
	}
	return string(p.raw[start:p.pos]), 's', nil
}

func (p *mapKVParser) skipSpaceAndComments() error {
	for p.pos < len(p.raw) {
		switch p.raw[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
			continue
		}
		if p.pos+1 < len(p.raw) && p.raw[p.pos] == '/' && p.raw[p.pos+1] == '/' {
			p.pos += 2
			for p.pos < len(p.raw) && p.raw[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		if p.pos+1 < len(p.raw) && p.raw[p.pos] == '/' && p.raw[p.pos+1] == '*' {
			end := bytes.Index(p.raw[p.pos+2:], []byte("*/"))
			if end < 0 {
				return errors.New("KeyValues block comment is unterminated")
			}
			p.pos += end + 4
			continue
		}
		break
	}
	return nil
}

func mapKVChild(pairs []mapKVPair, key string) (mapKVPair, bool) {
	for _, pair := range pairs {
		if strings.EqualFold(pair.Key, key) {
			return pair, true
		}
	}
	return mapKVPair{}, false
}

func mapKVValue(pairs []mapKVPair, key string) string {
	pair, ok := mapKVChild(pairs, key)
	if !ok || len(pair.Children) != 0 {
		return ""
	}
	return strings.TrimSpace(pair.Value)
}

func readMapEntryBounded(ctx context.Context, entry mapVPKEntry, limit int64) ([]byte, error) {
	if entry.size <= 0 || entry.size > limit {
		return nil, fmt.Errorf("VPK entry %q exceeds metadata size limit", entry.path)
	}
	reader, err := entry.open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	hash := crc32.NewIEEE()
	var body bytes.Buffer
	body.Grow(int(entry.size))
	written, err := io.Copy(io.MultiWriter(&body, hash), &mapContextReader{ctx: ctx, reader: reader})
	if err != nil {
		return nil, err
	}
	if written != entry.size || hash.Sum32() != entry.crc {
		return nil, fmt.Errorf("VPK checksum mismatch for %q", entry.path)
	}
	return body.Bytes(), nil
}

func inspectMapMissions(ctx context.Context, winners map[string]mapVPKEntry, bsps []mapLocalBSP, stagingRoot string) (mapMissionMetadata, mapLocalPoster, string, []mapLocalWarning) {
	candidates := make([]mapMissionMetadata, 0)
	warnings := make([]mapLocalWarning, 0)
	localization := mapMissionLocalization(ctx, winners)
	missionKeys := make([]string, 0)
	for key := range winners {
		if !strings.HasPrefix(key, "missions/") || pathpkg.Ext(key) != ".txt" || strings.Count(key, "/") != 1 {
			continue
		}
		missionKeys = append(missionKeys, key)
	}
	sort.Strings(missionKeys)
	for _, key := range missionKeys {
		entry := winners[key]
		raw, err := readMapEntryBounded(ctx, entry, maxMapMissionBytes)
		if err != nil {
			warnings = append(warnings, mapLocalWarning{Code: "invalid_mission_file", Message: err.Error(), VirtualPath: entry.path})
			continue
		}
		mission, err := parseMapMission(entry.path, raw, localization)
		if err != nil {
			warnings = append(warnings, mapLocalWarning{Code: "invalid_mission_file", Message: err.Error(), VirtualPath: entry.path})
			continue
		}
		candidates = append(candidates, mission)
	}
	if len(candidates) == 0 {
		warnings = append(warnings, mapLocalWarning{Code: "missing_mission_file", Message: "VPK does not contain a readable missions/*.txt file"})
		return mapMissionMetadata{}, mapLocalPoster{}, "", warnings
	}
	bspSet := make(map[string]bool, len(bsps))
	for _, bsp := range bsps {
		bspSet[strings.ToLower(bsp.StartName)] = true
	}
	score := func(candidate mapMissionMetadata) int {
		seen := make(map[string]bool)
		for _, mode := range candidate.Modes {
			for _, chapter := range mode.Chapters {
				key := strings.ToLower(chapter.BSPName)
				if bspSet[key] {
					seen[key] = true
				}
			}
		}
		return len(seen)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := score(candidates[i]), score(candidates[j])
		if left != right {
			return left > right
		}
		return strings.ToLower(candidates[i].Path) < strings.ToLower(candidates[j].Path)
	})
	selected := candidates[0]
	if len(candidates) > 1 {
		warnings = append(warnings, mapLocalWarning{Code: "multiple_mission_files", Message: fmt.Sprintf("Found %d mission files; selected %s by BSP coverage", len(candidates), selected.Path), VirtualPath: selected.Path})
	}
	referenced := make(map[string]bool)
	for _, mode := range selected.Modes {
		for _, chapter := range mode.Chapters {
			key := strings.ToLower(chapter.BSPName)
			referenced[key] = true
			if !bspSet[key] {
				warnings = append(warnings, mapLocalWarning{Code: "mission_missing_bsp", Message: "Mission chapter references a BSP that is not present", VirtualPath: "maps/" + chapter.BSPName + ".bsp"})
			}
		}
	}
	for _, bsp := range bsps {
		if bsp.StartName != "" && !referenced[strings.ToLower(bsp.StartName)] {
			warnings = append(warnings, mapLocalWarning{Code: "unreferenced_bsp", Message: "BSP is present but not referenced by the selected mission", VirtualPath: bsp.VirtualPath})
		}
	}
	poster, output, err := extractMapPoster(ctx, winners, selected.PosterReferences, stagingRoot)
	if err != nil {
		warnings = append(warnings, mapLocalWarning{Code: "poster_unavailable", Message: err.Error(), VirtualPath: selected.Path})
	}
	return selected, poster, output, warnings
}

func parseMapMission(path string, raw []byte, localization map[string]string) (mapMissionMetadata, error) {
	pairs, err := parseMapKeyValues(raw)
	if err != nil {
		return mapMissionMetadata{}, fmt.Errorf("parse mission: %w", err)
	}
	root, ok := mapKVChild(pairs, "mission")
	if !ok || len(root.Children) == 0 {
		return mapMissionMetadata{}, errors.New("mission file does not contain a mission object")
	}
	metadata := mapMissionMetadata{Path: path}
	metadata.Name = localizedMissionText(mapKVValue(root.Children, "DisplayTitle"), localization, 255)
	if metadata.Name == "" {
		metadata.Name = localizedMissionText(mapKVValue(root.Children, "Name"), localization, 255)
	}
	metadata.Version = safeMissionText(mapKVValue(root.Children, "Version"), 64)
	metadata.Author = safeMissionText(mapKVValue(root.Children, "Author"), 255)
	metadata.Description = localizedMissionText(mapKVValue(root.Children, "Description"), localization, 2048)
	if poster, found := mapKVChild(root.Children, "poster"); found {
		for _, key := range []string{"posterImage_widescreen", "posterImage"} {
			if value := safeMissionText(mapKVValue(poster.Children, key), 512); value != "" {
				metadata.PosterReferences = append(metadata.PosterReferences, value)
			}
		}
	}
	if imageRef := safeMissionText(mapKVValue(root.Children, "Image"), 512); imageRef != "" {
		metadata.PosterReferences = append(metadata.PosterReferences, imageRef)
	}
	modes, ok := mapKVChild(root.Children, "modes")
	if !ok || len(modes.Children) == 0 {
		return metadata, errors.New("mission file does not declare any modes")
	}
	seenModes := make(map[string]bool)
	for _, modePair := range modes.Children {
		modeName := strings.ToLower(strings.TrimSpace(modePair.Key))
		if !validSourceMapName(modeName) || len(modeName) > 64 || seenModes[modeName] {
			continue
		}
		seenModes[modeName] = true
		mode := mapLocalMode{Name: modeName}
		seenOrdinals := make(map[int]bool)
		for _, chapterPair := range modePair.Children {
			ordinal, err := strconv.Atoi(strings.TrimSpace(chapterPair.Key))
			if err != nil || ordinal < 1 || ordinal > 4096 || seenOrdinals[ordinal] {
				continue
			}
			bspName := normalizeMissionBSP(mapKVValue(chapterPair.Children, "Map"))
			if !validSourceMapName(bspName) {
				continue
			}
			seenOrdinals[ordinal] = true
			displayName := localizedMissionText(mapKVValue(chapterPair.Children, "DisplayName"), localization, 255)
			if displayName == "" {
				displayName = bspName
			}
			mode.Chapters = append(mode.Chapters, mapLocalChapter{Ordinal: ordinal, BSPName: bspName,
				DisplayName: displayName, Image: safeMissionText(mapKVValue(chapterPair.Children, "Image"), 512)})
		}
		sort.Slice(mode.Chapters, func(i, j int) bool { return mode.Chapters[i].Ordinal < mode.Chapters[j].Ordinal })
		if len(mode.Chapters) > 0 {
			metadata.Modes = append(metadata.Modes, mode)
		}
	}
	sort.SliceStable(metadata.Modes, func(i, j int) bool {
		left, right := mapModeRank(metadata.Modes[i].Name), mapModeRank(metadata.Modes[j].Name)
		if left != right {
			return left < right
		}
		return metadata.Modes[i].Name < metadata.Modes[j].Name
	})
	if len(metadata.Modes) == 0 {
		return metadata, errors.New("mission file does not contain a valid mode chapter")
	}
	return metadata, nil
}

func mapMissionLocalization(ctx context.Context, winners map[string]mapVPKEntry) map[string]string {
	keys := make([]string, 0)
	for key := range winners {
		if strings.HasPrefix(key, "resource/") && pathpkg.Ext(key) == ".txt" && strings.Count(key, "/") == 1 {
			keys = append(keys, key)
		}
	}
	languageRank := func(key string) int {
		for rank, language := range []string{"schinese", "tchinese", "english"} {
			if strings.Contains(key, "_"+language+".") {
				return rank
			}
		}
		return 3
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := languageRank(keys[i]), languageRank(keys[j])
		if left != right {
			return left < right
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 64 {
		keys = keys[:64]
	}
	tokens := make(map[string]string)
	var total int64
	for _, key := range keys {
		entry := winners[key]
		if total+entry.size > maxMapLocalization {
			continue
		}
		raw, err := readMapEntryBounded(ctx, entry, maxMapMissionBytes)
		if err != nil {
			continue
		}
		total += int64(len(raw))
		pairs, err := parseMapKeyValues(raw)
		if err != nil {
			continue
		}
		root, ok := mapKVChild(pairs, "lang")
		if !ok {
			continue
		}
		container, ok := mapKVChild(root.Children, "Tokens")
		if !ok {
			continue
		}
		for _, pair := range container.Children {
			name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(pair.Key), "#"))
			if name == "" || len(name) > 255 || len(pair.Children) != 0 {
				continue
			}
			if _, exists := tokens[name]; exists {
				continue
			}
			if value := safeMissionText(pair.Value, 2048); value != "" {
				tokens[name] = value
				if len(tokens) >= maxMapKVPairs {
					return tokens
				}
			}
		}
	}
	return tokens
}

func localizedMissionText(value string, localization map[string]string, limit int) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "#") {
		value = localization[strings.ToLower(strings.TrimPrefix(value, "#"))]
	}
	return safeMissionText(value, limit)
}

func mapModeRank(value string) int {
	for index, known := range []string{"coop", "realism", "versus", "survival", "scavenge"} {
		if value == known {
			return index
		}
	}
	return 100
}

func normalizeMissionBSP(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(strings.ToLower(value), "maps/")
	value = strings.TrimSuffix(value, ".bsp")
	return value
}

func safeMissionText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return truncateUTF8(value, limit)
}

func extractMapPoster(ctx context.Context, winners map[string]mapVPKEntry, references []string, stagingRoot string) (mapLocalPoster, string, error) {
	var lastErr error
	seen := make(map[string]bool)
	for _, reference := range references {
		for _, candidate := range posterVTFCandidates(ctx, winners, reference) {
			key := strings.ToLower(candidate)
			if seen[key] {
				continue
			}
			seen[key] = true
			entry, ok := winners[key]
			if !ok {
				continue
			}
			raw, err := readMapEntryBounded(ctx, entry, maxMapPosterVTF)
			if err != nil {
				lastErr = err
				continue
			}
			jpegBody, err := decodeMapPosterVTF(raw)
			if err != nil {
				lastErr = fmt.Errorf("decode %s: %w", entry.path, err)
				continue
			}
			output, err := ResolveWithin(stagingRoot, "poster.jpg")
			if err != nil {
				return mapLocalPoster{}, "", err
			}
			if err := os.WriteFile(output, jpegBody, 0o600); err != nil {
				return mapLocalPoster{}, "", err
			}
			sha, size, err := hashFile(output)
			if err != nil {
				return mapLocalPoster{}, "", err
			}
			return mapLocalPoster{SHA256: sha, Size: size, MIME: "image/jpeg"}, output, nil
		}
	}
	if lastErr != nil {
		return mapLocalPoster{}, "", lastErr
	}
	return mapLocalPoster{}, "", errors.New("mission poster material is missing from the VPK")
}

func posterVTFCandidates(ctx context.Context, winners map[string]mapVPKEntry, reference string) []string {
	normalized := normalizeMaterialReference(reference)
	if normalized == "" {
		return nil
	}
	base := normalized
	if !strings.HasPrefix(base, "vgui/") {
		base = "vgui/" + base
	}
	candidates := []string{"materials/" + base + ".vtf"}
	if entry, ok := winners[strings.ToLower("materials/"+base+".vmt")]; ok {
		if raw, err := readMapEntryBounded(ctx, entry, maxMapVMTBytes); err == nil {
			if pairs, err := parseMapKeyValues(raw); err == nil {
				if texture := findMapKVScalar(pairs, "$basetexture"); texture != "" {
					if texture = normalizeMaterialReference(texture); texture != "" {
						candidates = append([]string{"materials/" + texture + ".vtf"}, candidates...)
					}
				}
			}
		}
	}
	return candidates
}

func normalizeMaterialReference(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	value = strings.TrimPrefix(value, "materials/")
	value = strings.TrimSuffix(strings.TrimSuffix(value, ".vmt"), ".vtf")
	value = pathpkg.Clean(value)
	if value == "." || value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "../") || strings.Contains(value, "//") || len(value) > 512 {
		return ""
	}
	return value
}

func findMapKVScalar(pairs []mapKVPair, key string) string {
	for _, pair := range pairs {
		if strings.EqualFold(pair.Key, key) && len(pair.Children) == 0 {
			return strings.TrimSpace(pair.Value)
		}
		if value := findMapKVScalar(pair.Children, key); value != "" {
			return value
		}
	}
	return ""
}

func decodeMapPosterVTF(raw []byte) (result []byte, err error) {
	header, err := validateMapPosterVTF(raw)
	if err != nil {
		return nil, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("invalid VTF poster data: %v", recovered)
		}
	}()
	packed := raw[header.highResolutionAt : header.highResolutionAt+header.highResolutionSize]
	decoded, err := decodeMapVTFPixels(header.width, header.height, header.format, packed)
	if err != nil {
		return nil, err
	}
	targetWidth, targetHeight := header.width, header.height
	const maxWidth, maxHeight = 1280, 720
	if targetWidth > maxWidth || targetHeight > maxHeight {
		ratio := min(float64(maxWidth)/float64(targetWidth), float64(maxHeight)/float64(targetHeight))
		targetWidth = max(1, int(float64(targetWidth)*ratio))
		targetHeight = max(1, int(float64(targetHeight)*ratio))
	}
	scaled := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), decoded, decoded.Bounds(), draw.Src, nil)
	opaque := image.NewRGBA(scaled.Bounds())
	draw.Draw(opaque, opaque.Bounds(), &image.Uniform{C: color.RGBA{R: 12, G: 21, B: 24, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(opaque, opaque.Bounds(), scaled, image.Point{}, draw.Over)
	for _, quality := range []int{84, 72, 60} {
		var output bytes.Buffer
		if err := jpeg.Encode(&output, opaque, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if output.Len() <= maxMapPosterJPEG {
			return output.Bytes(), nil
		}
	}
	return nil, errors.New("converted poster exceeds 1 MiB")
}

type mapVTFHeader struct {
	width              int
	height             int
	format             uint32
	highResolutionAt   int64
	highResolutionSize int64
}

func validateMapPosterVTF(raw []byte) (mapVTFHeader, error) {
	if len(raw) < 65 || string(raw[:4]) != "VTF\x00" {
		return mapVTFHeader{}, errors.New("VTF poster header is invalid")
	}
	major, minor := binary.LittleEndian.Uint32(raw[4:8]), binary.LittleEndian.Uint32(raw[8:12])
	if major != 7 || minor > 5 {
		return mapVTFHeader{}, fmt.Errorf("VTF poster version %d.%d is unsupported", major, minor)
	}
	headerSize := int64(binary.LittleEndian.Uint32(raw[12:16]))
	width, height := int(binary.LittleEndian.Uint16(raw[16:18])), int(binary.LittleEndian.Uint16(raw[18:20]))
	flags := binary.LittleEndian.Uint32(raw[20:24])
	frames := binary.LittleEndian.Uint16(raw[24:26])
	format := binary.LittleEndian.Uint32(raw[52:56])
	mipmaps := int(raw[56])
	lowWidth, lowHeight := int(raw[61]), int(raw[62])
	depth := uint16(1)
	if minor >= 2 {
		depth = binary.LittleEndian.Uint16(raw[63:65])
	}
	if headerSize < 64 || headerSize > 1024 || headerSize > int64(len(raw)) {
		return mapVTFHeader{}, errors.New("VTF poster header size is invalid")
	}
	if width <= 0 || height <= 0 || width > 4096 || height > 4096 || int64(width)*int64(height) > 16<<20 || frames != 1 || depth > 1 || flags&0x4000 != 0 {
		return mapVTFHeader{}, errors.New("VTF poster dimensions, frames, or texture type are unsupported")
	}
	maxMipmaps := 0
	for dimension := max(width, height); dimension > 0; dimension >>= 1 {
		maxMipmaps++
	}
	if mipmaps < 1 || mipmaps > maxMipmaps {
		return mapVTFHeader{}, errors.New("VTF poster mipmap count is invalid")
	}
	if lowWidth > 16 || lowHeight > 16 || (lowWidth == 0) != (lowHeight == 0) {
		return mapVTFHeader{}, errors.New("VTF poster thumbnail dimensions are invalid")
	}
	lowSize := int64(0)
	if lowWidth != 0 {
		lowSize = int64((lowWidth+3)/4) * int64((lowHeight+3)/4) * 8
	}
	lowResolutionAt := headerSize
	highResolutionAt := headerSize + lowSize
	if minor >= 3 {
		if len(raw) < 80 {
			return mapVTFHeader{}, errors.New("VTF poster resource header is truncated")
		}
		resourceCount := int(binary.LittleEndian.Uint32(raw[68:72]))
		if resourceCount < 1 || resourceCount > 32 || int64(80+resourceCount*8) > headerSize {
			return mapVTFHeader{}, errors.New("VTF poster resource table is invalid")
		}
		lowResolutionAt, highResolutionAt = -1, -1
		for index := range resourceCount {
			offset := 80 + index*8
			tag, flags := raw[offset:offset+3], raw[offset+3]
			if flags&0x02 != 0 {
				continue
			}
			dataOffset := int64(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
			switch {
			case tag[0] == 0x01 && tag[1] == 0 && tag[2] == 0:
				lowResolutionAt = dataOffset
			case tag[0] == 0x30 && tag[1] == 0 && tag[2] == 0:
				highResolutionAt = dataOffset
			}
		}
		if highResolutionAt < headerSize || (lowSize > 0 && lowResolutionAt < headerSize) {
			return mapVTFHeader{}, errors.New("VTF poster image resource is missing")
		}
	}
	if lowSize > 0 && (lowResolutionAt < 0 || lowResolutionAt+lowSize > int64(len(raw))) {
		return mapVTFHeader{}, errors.New("VTF poster thumbnail is truncated")
	}
	total := int64(0)
	mipWidth, mipHeight := width, height
	highResolutionSize := int64(0)
	for range mipmaps {
		size, err := mapVTFPackedSize(mipWidth, mipHeight, format)
		if err != nil {
			return mapVTFHeader{}, err
		}
		if highResolutionSize == 0 {
			highResolutionSize = size
		}
		total += size
		mipWidth, mipHeight = max(1, mipWidth/2), max(1, mipHeight/2)
	}
	if highResolutionAt < headerSize || total > int64(len(raw))-highResolutionAt {
		return mapVTFHeader{}, errors.New("VTF poster mipmap data is truncated")
	}
	return mapVTFHeader{width: width, height: height, format: format,
		highResolutionAt: highResolutionAt + total - highResolutionSize, highResolutionSize: highResolutionSize}, nil
}

func mapVTFPackedSize(width, height int, format uint32) (int64, error) {
	pixels := int64(width) * int64(height)
	switch format {
	case 0, 12, 16:
		return pixels * 4, nil
	case 2, 3, 9, 10:
		return pixels * 3, nil
	case 13, 20:
		return int64((width+3)/4) * int64((height+3)/4) * 8, nil
	case 14, 15:
		return int64((width+3)/4) * int64((height+3)/4) * 16, nil
	default:
		return 0, fmt.Errorf("VTF image format %d is unsupported for posters", format)
	}
}

func decodeMapVTFPixels(width, height int, format uint32, packed []byte) (*image.RGBA, error) {
	imageOut := image.NewRGBA(image.Rect(0, 0, width, height))
	set := func(index, r, g, b, a int) {
		pixel := index * 4
		imageOut.Pix[pixel], imageOut.Pix[pixel+1], imageOut.Pix[pixel+2], imageOut.Pix[pixel+3] = byte(r), byte(g), byte(b), byte(a)
	}
	pixels := width * height
	need := func(bytesPerPixel int) error {
		if len(packed) != pixels*bytesPerPixel {
			return errors.New("VTF pixel data length is inconsistent")
		}
		return nil
	}
	switch format {
	case 0: // RGBA8888
		if err := need(4); err != nil {
			return nil, err
		}
		copy(imageOut.Pix, packed)
	case 12: // BGRA8888
		if err := need(4); err != nil {
			return nil, err
		}
		for i := range pixels {
			set(i, int(packed[i*4+2]), int(packed[i*4+1]), int(packed[i*4]), int(packed[i*4+3]))
		}
	case 16: // BGRX8888
		if err := need(4); err != nil {
			return nil, err
		}
		for i := range pixels {
			set(i, int(packed[i*4+2]), int(packed[i*4+1]), int(packed[i*4]), 255)
		}
	case 2, 9: // RGB888 / bluescreen
		if err := need(3); err != nil {
			return nil, err
		}
		for i := range pixels {
			set(i, int(packed[i*3]), int(packed[i*3+1]), int(packed[i*3+2]), 255)
		}
	case 3, 10: // BGR888 / bluescreen
		if err := need(3); err != nil {
			return nil, err
		}
		for i := range pixels {
			set(i, int(packed[i*3+2]), int(packed[i*3+1]), int(packed[i*3]), 255)
		}
	case 13, 20:
		if err := decodeMapDXT(imageOut, packed, 1); err != nil {
			return nil, err
		}
	case 14:
		if err := decodeMapDXT(imageOut, packed, 3); err != nil {
			return nil, err
		}
	case 15:
		if err := decodeMapDXT(imageOut, packed, 5); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("VTF image format %d is unsupported for posters", format)
	}
	return imageOut, nil
}

func decodeMapDXT(target *image.RGBA, packed []byte, variant int) error {
	width, height := target.Bounds().Dx(), target.Bounds().Dy()
	blockBytes := 8
	if variant == 3 || variant == 5 {
		blockBytes = 16
	}
	blocksX, blocksY := (width+3)/4, (height+3)/4
	if len(packed) != blocksX*blocksY*blockBytes {
		return errors.New("VTF DXT data length is inconsistent")
	}
	offset := 0
	for by := 0; by < blocksY; by++ {
		for bx := 0; bx < blocksX; bx++ {
			block := packed[offset : offset+blockBytes]
			offset += blockBytes
			colorOffset := 0
			if variant == 3 || variant == 5 {
				colorOffset = 8
			}
			c0 := uint16(block[colorOffset]) | uint16(block[colorOffset+1])<<8
			c1 := uint16(block[colorOffset+2]) | uint16(block[colorOffset+3])<<8
			palette := dxtColorPalette(c0, c1, variant == 1)
			codes := uint32(block[colorOffset+4]) | uint32(block[colorOffset+5])<<8 | uint32(block[colorOffset+6])<<16 | uint32(block[colorOffset+7])<<24
			for py := 0; py < 4; py++ {
				for px := 0; px < 4; px++ {
					x, y := bx*4+px, by*4+py
					if x >= width || y >= height {
						continue
					}
					pixelIndex := py*4 + px
					selected := palette[(codes>>uint(pixelIndex*2))&3]
					alpha := selected.A
					if variant == 3 {
						alphaByte := block[pixelIndex/2]
						alpha4 := alphaByte >> uint((pixelIndex%2)*4) & 0x0f
						alpha = alpha4 * 17
					} else if variant == 5 {
						alpha = dxt5Alpha(block[:8], pixelIndex)
					}
					target.SetRGBA(x, y, color.RGBA{R: selected.R, G: selected.G, B: selected.B, A: alpha})
				}
			}
		}
	}
	return nil
}

func dxtColorPalette(c0, c1 uint16, allowTransparent bool) [4]color.RGBA {
	decode := func(value uint16) color.RGBA {
		r := uint8((value >> 11) & 31)
		g := uint8((value >> 5) & 63)
		b := uint8(value & 31)
		return color.RGBA{R: (r << 3) | (r >> 2), G: (g << 2) | (g >> 4), B: (b << 3) | (b >> 2), A: 255}
	}
	result := [4]color.RGBA{decode(c0), decode(c1)}
	if c0 > c1 || !allowTransparent {
		result[2] = color.RGBA{R: uint8((2*uint16(result[0].R) + uint16(result[1].R)) / 3), G: uint8((2*uint16(result[0].G) + uint16(result[1].G)) / 3), B: uint8((2*uint16(result[0].B) + uint16(result[1].B)) / 3), A: 255}
		result[3] = color.RGBA{R: uint8((uint16(result[0].R) + 2*uint16(result[1].R)) / 3), G: uint8((uint16(result[0].G) + 2*uint16(result[1].G)) / 3), B: uint8((uint16(result[0].B) + 2*uint16(result[1].B)) / 3), A: 255}
	} else {
		result[2] = color.RGBA{R: uint8((uint16(result[0].R) + uint16(result[1].R)) / 2), G: uint8((uint16(result[0].G) + uint16(result[1].G)) / 2), B: uint8((uint16(result[0].B) + uint16(result[1].B)) / 2), A: 255}
		result[3] = color.RGBA{}
	}
	return result
}

func dxt5Alpha(block []byte, pixel int) uint8 {
	a0, a1 := block[0], block[1]
	bits := uint64(0)
	for index := 0; index < 6; index++ {
		bits |= uint64(block[2+index]) << uint(index*8)
	}
	code := uint8((bits >> uint(pixel*3)) & 7)
	if code == 0 {
		return a0
	}
	if code == 1 {
		return a1
	}
	if a0 > a1 {
		return uint8(((8-int(code))*int(a0) + (int(code)-1)*int(a1)) / 7)
	}
	if code == 6 {
		return 0
	}
	if code == 7 {
		return 255
	}
	return uint8(((6-int(code))*int(a0) + (int(code)-1)*int(a1)) / 5)
}
