package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const L4D2DedicatedAppID = 222860

const maxSteamAppInfoOutput = 16 << 20

type SteamCMD struct {
	path string
	sem  chan struct{}
}

func NewSteamCMD(path string, concurrency int) *SteamCMD {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &SteamCMD{path: path, sem: make(chan struct{}, concurrency)}
}

func (s *SteamCMD) Available() error {
	if s.path == "" {
		return errors.New("steamcmd_path is not configured")
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("SteamCMD not found: %w", err)
	}
	if st.IsDir() {
		return errors.New("steamcmd_path points to a directory")
	}
	return nil
}

func (s *SteamCMD) Install(ctx context.Context, installDir string, appID int, validate bool, emit func(int, string)) error {
	if err := s.Available(); err != nil {
		return err
	}
	if appID <= 0 {
		return errors.New("invalid Steam app id")
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()
	args := BuildSteamCMDArgs(installDir, appID, validate)
	cmd := exec.CommandContext(ctx, s.path, args...)
	cmd.Dir = filepathDir(s.path)
	cmd.Stdin = nil
	cmd.Env = restrictedSteamEnvironment(filepathDir(s.path))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	errLines := make(chan []string, 1)
	go func() { errLines <- scanSteamOutput(stderr, nil) }()
	lines := scanSteamOutput(stdout, emit)
	lines = append(lines, <-errLines...)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("SteamCMD failed: %w: %s", err, lastOutput(lines, 20))
	}
	joined := strings.ToLower(strings.Join(lines, "\n"))
	if strings.Contains(joined, "error!") || strings.Contains(joined, "failed to install app") {
		return fmt.Errorf("SteamCMD reported failure: %s", lastOutput(lines, 20))
	}
	if emit != nil {
		emit(100, "SteamCMD completed")
	}
	return nil
}

// PublicBuildID asks SteamCMD for the current public branch metadata without
// installing or changing an app. Valve's app_info_print output is VDF embedded
// in ordinary log output, so parsing deliberately ignores text outside the
// requested app object and requires depots/branches/public/buildid.
func (s *SteamCMD) PublicBuildID(ctx context.Context, appID int) (string, error) {
	if err := s.Available(); err != nil {
		return "", err
	}
	if appID <= 0 {
		return "", errors.New("invalid Steam app id")
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.sem }()

	args := BuildSteamAppInfoArgs(appID)
	cmd := exec.CommandContext(ctx, s.path, args...)
	cmd.Dir = filepath.Dir(s.path)
	cmd.Stdin = nil
	cmd.Env = restrictedSteamEnvironment(filepath.Dir(s.path))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	type readResult struct {
		data     []byte
		err      error
		exceeded bool
	}
	read := func(reader io.Reader) readResult {
		capture := &boundedCapture{limit: maxSteamAppInfoOutput}
		_, readErr := io.Copy(capture, reader)
		return readResult{data: capture.Bytes(), err: readErr, exceeded: capture.exceeded}
	}
	stdoutResult := make(chan readResult, 1)
	stderrResult := make(chan readResult, 1)
	go func() { stdoutResult <- read(stdout) }()
	go func() { stderrResult <- read(stderr) }()
	out := <-stdoutResult
	errOut := <-stderrResult
	waitErr := cmd.Wait()
	if out.err != nil {
		return "", out.err
	}
	if errOut.err != nil {
		return "", errOut.err
	}
	if out.exceeded || errOut.exceeded {
		return "", errors.New("SteamCMD app info output exceeds safety limit")
	}
	combined := append(append([]byte(nil), out.data...), errOut.data...)
	if waitErr != nil {
		return "", fmt.Errorf("SteamCMD app info query failed: %w: %s", waitErr, lastOutput(bytesToLines(combined), 20))
	}
	buildID, err := ParseSteamAppPublicBuildID(combined, appID)
	if err != nil {
		return "", fmt.Errorf("parse SteamCMD public build: %w", err)
	}
	return buildID, nil
}

// InstalledBuildID checks the instance-local manifest first. The SteamCMD-root
// candidate accommodates installations produced by older SteamCMD layouts;
// callers receive only a source label so host paths are not exposed upstream.
func (s *SteamCMD) InstalledBuildID(installDir string, appID int) (buildID, source string, err error) {
	if appID <= 0 {
		return "", "", errors.New("invalid Steam app id")
	}
	name := "appmanifest_" + strconv.Itoa(appID) + ".acf"
	instanceCandidate, pathErr := ResolveWithin(installDir, filepath.Join("steamapps", name))
	if pathErr != nil {
		return "", "", pathErr
	}
	candidates := []struct {
		path   string
		source string
	}{{instanceCandidate, "instance"}}
	if s.path != "" {
		steamRoot := filepath.Dir(s.path)
		steamCandidate, resolveErr := ResolveWithin(steamRoot, filepath.Join("steamapps", name))
		if resolveErr == nil && !samePath(steamCandidate, instanceCandidate) {
			candidates = append(candidates, struct {
				path   string
				source string
			}{steamCandidate, "steamcmd"})
		}
	}
	var malformed []string
	for _, candidate := range candidates {
		raw, readErr := os.ReadFile(candidate.path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read installed app manifest: %w", readErr)
		}
		build, parseErr := ParseSteamAppManifestBuildID(raw, appID)
		if parseErr == nil {
			return build, candidate.source, nil
		}
		malformed = append(malformed, candidate.source+": "+parseErr.Error())
	}
	if len(malformed) > 0 {
		return "", "", fmt.Errorf("installed app manifest is invalid (%s)", strings.Join(malformed, "; "))
	}
	return "", "", os.ErrNotExist
}

func ParseSteamAppPublicBuildID(raw []byte, appID int) (string, error) {
	raw = sliceFromVDFKey(raw, strconv.Itoa(appID))
	tokens, err := tokenizeVDF(raw)
	if err != nil {
		return "", err
	}
	object, err := findVDFObject(tokens, strconv.Itoa(appID))
	if err != nil {
		return "", errors.New("requested app object not found")
	}
	buildID := nestedVDFString(object, "depots", "branches", "public", "buildid")
	if !validBuildID(buildID) {
		return "", errors.New("public branch buildid not found or invalid")
	}
	return buildID, nil
}

func ParseSteamAppManifestBuildID(raw []byte, appID int) (string, error) {
	raw = sliceFromVDFKey(raw, "AppState")
	tokens, err := tokenizeVDF(raw)
	if err != nil {
		return "", err
	}
	object, err := findVDFObject(tokens, "AppState")
	if err != nil {
		return "", errors.New("AppState object not found")
	}
	if nestedVDFString(object, "appid") != strconv.Itoa(appID) {
		return "", errors.New("manifest appid does not match")
	}
	buildID := nestedVDFString(object, "buildid")
	if !validBuildID(buildID) {
		return "", errors.New("manifest buildid not found or invalid")
	}
	return buildID, nil
}

func BuildSteamAppInfoArgs(appID int) []string {
	return []string{"+login", "anonymous", "+app_info_update", "1", "+app_info_print", strconv.Itoa(appID), "+quit"}
}

func sliceFromVDFKey(raw []byte, key string) []byte {
	marker := []byte(`"` + key + `"`)
	if index := bytes.Index(raw, marker); index >= 0 {
		return raw[index:]
	}
	return raw
}

type vdfObject map[string]any

func tokenizeVDF(raw []byte) ([]string, error) {
	tokens := make([]string, 0, 256)
	for i := 0; i < len(raw); {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '{', '}':
			tokens = append(tokens, string(raw[i]))
			i++
		case '"':
			i++
			var value strings.Builder
			closed := false
			for i < len(raw) {
				if raw[i] == '"' {
					i++
					closed = true
					break
				}
				if raw[i] == '\\' && i+1 < len(raw) && (raw[i+1] == '\\' || raw[i+1] == '"') {
					i++
				}
				value.WriteByte(raw[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated quoted VDF string")
			}
			tokens = append(tokens, value.String())
		default:
			start := i
			for i < len(raw) && !bytes.ContainsRune([]byte(" \t\r\n{}"), rune(raw[i])) {
				i++
			}
			tokens = append(tokens, string(raw[start:i]))
		}
	}
	return tokens, nil
}

func findVDFObject(tokens []string, key string) (vdfObject, error) {
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] != key || tokens[i+1] != "{" {
			continue
		}
		object, _, err := parseVDFObject(tokens, i+2)
		if err == nil {
			return object, nil
		}
	}
	return nil, errors.New("VDF object not found")
}

func parseVDFObject(tokens []string, index int) (vdfObject, int, error) {
	object := make(vdfObject)
	for index < len(tokens) {
		if tokens[index] == "}" {
			return object, index + 1, nil
		}
		key := tokens[index]
		index++
		if index >= len(tokens) {
			return nil, index, errors.New("VDF value missing")
		}
		if tokens[index] == "{" {
			child, next, err := parseVDFObject(tokens, index+1)
			if err != nil {
				return nil, next, err
			}
			object[key] = child
			index = next
			continue
		}
		if tokens[index] == "}" {
			return nil, index, errors.New("VDF value missing before closing brace")
		}
		object[key] = tokens[index]
		index++
	}
	return nil, index, errors.New("unterminated VDF object")
}

func nestedVDFString(object vdfObject, path ...string) string {
	var current any = object
	for index, key := range path {
		mapped, ok := current.(vdfObject)
		if !ok {
			return ""
		}
		value, ok := mapped[key]
		if !ok {
			return ""
		}
		if index == len(path)-1 {
			text, _ := value.(string)
			return text
		}
		current = value
	}
	return ""
}

func validBuildID(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func bytesToLines(raw []byte) []string {
	return strings.FieldsFunc(string(raw), func(char rune) bool { return char == '\r' || char == '\n' })
}

type boundedCapture struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (capture *boundedCapture) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := capture.limit - capture.buffer.Len()
	if remaining < len(data) {
		capture.exceeded = true
		if remaining < 0 {
			remaining = 0
		}
		data = data[:remaining]
	}
	if len(data) > 0 {
		_, _ = capture.buffer.Write(data)
	}
	return originalLength, nil
}

func (capture *boundedCapture) Bytes() []byte { return capture.buffer.Bytes() }

func (s *SteamCMD) WorkshopItem(ctx context.Context, itemID string, emit func(int, string)) ([]string, error) {
	if err := s.Available(); err != nil {
		return nil, err
	}
	if err := validateWorkshopID(itemID); err != nil {
		return nil, err
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.sem }()
	args := []string{"+login", "anonymous", "+workshop_download_item", "550", itemID, "+quit"}
	cmd := exec.CommandContext(ctx, s.path, args...)
	cmd.Dir = filepath.Dir(s.path)
	cmd.Stdin = nil
	cmd.Env = restrictedSteamEnvironment(filepath.Dir(s.path))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	errLines := make(chan []string, 1)
	go func() { errLines <- scanSteamOutput(stderr, nil) }()
	lines := scanSteamOutput(stdout, emit)
	lines = append(lines, <-errLines...)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("SteamCMD Workshop download failed: %w: %s", err, lastOutput(lines, 20))
	}
	files, err := findWorkshopVPKFiles(filepath.Dir(s.path), itemID)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		emit(100, "Workshop item downloaded")
	}
	return files, nil
}

// SteamCMD uses two Workshop layouts across the supported platforms. Windows
// normally writes below steamapps directly, while Linux legacy Workshop depots
// are written below $HOME/Steam/steamapps. The Agent fixes HOME to steamRoot,
// so both candidates remain beneath the configured SteamCMD directory.
func findWorkshopVPKFiles(steamRoot, itemID string) ([]string, error) {
	if err := validateWorkshopID(itemID); err != nil {
		return nil, err
	}
	relativeRoots := []string{
		filepath.Join("steamapps", "workshop", "content", "550", itemID),
		filepath.Join("Steam", "steamapps", "workshop", "content", "550", itemID),
	}
	if runtime.GOOS != "windows" {
		relativeRoots[0], relativeRoots[1] = relativeRoots[1], relativeRoots[0]
	}
	foundRoot := false
	for _, relativeRoot := range relativeRoots {
		root, err := ResolveWithin(steamRoot, relativeRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve Workshop content directory: %w", err)
		}
		if _, err := os.Lstat(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		foundRoot = true
		files := make([]string, 0)
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("Workshop download contains a symlink")
			}
			if entry.IsDir() || !isWorkshopVPKCandidate(entry.Name()) {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			sort.Strings(files)
			return files, nil
		}
	}
	if !foundRoot {
		return nil, errors.New("Workshop content directory was not created by SteamCMD")
	}
	return nil, errors.New("Workshop item did not contain a VPK")
}

func isWorkshopVPKCandidate(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".vpk") || strings.HasSuffix(lower, "_legacy.bin")
}

func BuildSteamCMDArgs(installDir string, appID int, validate bool) []string {
	args := []string{"+force_install_dir", installDir, "+login", "anonymous", "+app_update", strconv.Itoa(appID)}
	if validate {
		args = append(args, "validate")
	}
	return append(args, "+quit")
}

var percentPattern = regexp.MustCompile(`(?i)(?:progress|percent)[^0-9]{0,8}([0-9]{1,3})(?:\.[0-9]+)?%?`)

func scanSteamOutput(reader io.Reader, emit func(int, string)) []string {
	lines := make([]string, 0, 64)
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		if len(lines) >= 200 {
			copy(lines, lines[1:])
			lines[len(lines)-1] = line
		} else {
			lines = append(lines, line)
		}
		if emit != nil {
			progress := 0
			if match := percentPattern.FindStringSubmatch(line); len(match) == 2 {
				progress, _ = strconv.Atoi(match[1])
				if progress > 99 {
					progress = 99
				}
			}
			emit(progress, line)
		}
	}
	return lines
}

func lastOutput(lines []string, count int) string {
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}

func filepathDir(path string) string {
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 {
		return "."
	}
	return path[:idx]
}
