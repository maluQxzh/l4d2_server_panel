package agent

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const maxBackupManifestBytes = 4 << 20

// BackupLaunchConfig is the secret-free, node-port-independent subset of an
// instance configuration needed to reproduce its startup behavior elsewhere.
// RCON and server passwords are intentionally not representable in this type;
// restore combines these fields with the target instance's encrypted secrets.
type BackupLaunchConfig struct {
	Hostname     string            `json:"hostname,omitempty"`
	Mode         string            `json:"mode"`
	Difficulty   string            `json:"difficulty,omitempty"`
	MaxPlayers   int               `json:"max_players"`
	StartMap     string            `json:"start_map"`
	Region       string            `json:"region,omitempty"`
	SteamGroupID string            `json:"steam_group_id,omitempty"`
	ExtraArgs    []string          `json:"extra_args,omitempty"`
	CVars        map[string]string `json:"cvars,omitempty"`
	AutoRestart  bool              `json:"auto_restart"`
}

func readBackupManifestEntries(files []*zip.File) (BackupManifest, error) {
	if len(files) > maxBackupFiles+1 {
		return BackupManifest{}, errors.New("backup contains too many entries")
	}
	var entry *zip.File
	for _, file := range files {
		if file.Name != "manifest.json" {
			continue
		}
		if entry != nil {
			return BackupManifest{}, errors.New("backup contains duplicate manifests")
		}
		if file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() {
			return BackupManifest{}, errors.New("backup manifest is not a regular file")
		}
		entry = file
	}
	if entry == nil {
		return BackupManifest{}, errors.New("backup manifest is missing")
	}
	reader, err := entry.Open()
	if err != nil {
		return BackupManifest{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, maxBackupManifestBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return BackupManifest{}, readErr
	}
	if closeErr != nil {
		return BackupManifest{}, closeErr
	}
	if len(raw) > maxBackupManifestBytes {
		return BackupManifest{}, errors.New("backup manifest exceeds 4 MiB")
	}
	return decodeBackupManifest(raw)
}

func decodeBackupManifest(raw []byte) (BackupManifest, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return BackupManifest{}, errors.New("invalid backup manifest")
	}
	if err := validateBackupJSONFieldNames(raw); err != nil {
		return BackupManifest{}, errors.New("invalid backup manifest")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest BackupManifest
	if err := decoder.Decode(&manifest); err != nil {
		return BackupManifest{}, errors.New("invalid backup manifest")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BackupManifest{}, errors.New("invalid backup manifest")
	}
	if err := validateBackupManifest(manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("unsupported or invalid backup manifest: %w", err)
	}
	return manifest, nil
}

func validateBackupJSONFieldNames(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	allowedRoot := map[string]bool{
		"version": true, "instance_id": true, "os": true, "arch": true,
		"created_at": true, "files": true, "launch": true,
	}
	for key := range root {
		if !allowedRoot[key] {
			return fmt.Errorf("unknown manifest field %q", key)
		}
	}
	launchRaw, exists := root["launch"]
	if !exists || bytes.Equal(bytes.TrimSpace(launchRaw), []byte("null")) {
		return nil
	}
	var launch map[string]json.RawMessage
	if err := json.Unmarshal(launchRaw, &launch); err != nil {
		return err
	}
	allowedLaunch := map[string]bool{
		"hostname": true, "mode": true, "difficulty": true, "max_players": true,
		"start_map": true, "region": true, "steam_group_id": true, "extra_args": true,
		"cvars": true, "auto_restart": true,
	}
	for key := range launch {
		if !allowedLaunch[key] {
			return fmt.Errorf("unknown launch field %q", key)
		}
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("duplicate or invalid JSON object key")
				}
				seen[key] = true
				if err := visit(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := visit(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := visit(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func validateBackupManifest(manifest BackupManifest) error {
	if manifest.Version != backupLegacyFormatVersion && manifest.Version != backupFormatVersion {
		return errors.New("unsupported format version")
	}
	if manifest.Version == backupLegacyFormatVersion && manifest.Launch != nil {
		return errors.New("legacy manifest cannot contain launch configuration")
	}
	if manifest.Version == backupFormatVersion {
		if manifest.Launch == nil {
			return errors.New("v2 manifest is missing launch configuration")
		}
		if err := validateBackupLaunchConfig(*manifest.Launch); err != nil {
			return fmt.Errorf("launch configuration: %w", err)
		}
	}
	if err := validateManifestText("instance_id", manifest.InstanceID, 128, false); err != nil {
		return err
	}
	if !sourceIdentifier.MatchString(manifest.InstanceID) {
		return errors.New("instance_id must be a safe identifier")
	}
	if manifest.OS != "windows" && manifest.OS != "linux" {
		return errors.New("unsupported source operating system")
	}
	if manifest.Arch != "amd64" {
		return errors.New("unsupported source architecture")
	}
	if manifest.CreatedAt.IsZero() {
		return errors.New("created_at is required")
	}
	if manifest.Files == nil || len(manifest.Files) > maxBackupFiles {
		return errors.New("invalid file manifest")
	}
	portableNames := make(map[string]string, len(manifest.Files))
	for relative, checksum := range manifest.Files {
		cleaned, err := cleanBackupRelative(relative)
		if err != nil || cleaned != relative || !utf8.ValidString(relative) || strings.ContainsAny(relative, "\x00\r\n") || isGeneratedSecretConfig(cleaned) {
			return fmt.Errorf("invalid portable file path %q", relative)
		}
		folded := strings.ToLower(cleaned)
		if previous, exists := portableNames[folded]; exists && previous != cleaned {
			return fmt.Errorf("portable paths %q and %q collide on Windows", previous, cleaned)
		}
		portableNames[folded] = cleaned
		if len(checksum) != 64 {
			return fmt.Errorf("invalid SHA-256 for %q", cleaned)
		}
		if _, err := hex.DecodeString(checksum); err != nil {
			return fmt.Errorf("invalid SHA-256 for %q", cleaned)
		}
	}
	return nil
}

func validateBackupLaunchConfig(config BackupLaunchConfig) error {
	if config.MaxPlayers < 1 || config.MaxPlayers > 32 {
		return errors.New("max_players must be between 1 and 32")
	}
	if !sourceIdentifier.MatchString(config.Mode) || !sourceIdentifier.MatchString(config.StartMap) {
		return errors.New("mode and start_map must be safe Source identifiers")
	}
	for field, value := range map[string]string{
		"hostname": config.Hostname, "region": config.Region, "steam_group_id": config.SteamGroupID,
	} {
		if err := validateManifestText(field, value, 256, true); err != nil {
			return err
		}
	}
	if config.Difficulty != "" {
		switch strings.ToLower(config.Difficulty) {
		case "easy", "normal", "hard", "impossible":
		default:
			return errors.New("difficulty must be easy, normal, hard, or impossible")
		}
	}
	if config.SteamGroupID != "" && (len(config.SteamGroupID) > 20 || strings.Trim(config.SteamGroupID, "0123456789") != "") {
		return errors.New("steam_group_id must contain at most 20 decimal digits")
	}
	if err := validateExtraStartArgs(config.ExtraArgs); err != nil {
		return err
	}
	if len(config.CVars) > 256 {
		return errors.New("configuration contains too many custom CVars")
	}
	seenCVars := make(map[string]bool, len(config.CVars))
	reservedCVars := map[string]bool{"hostname": true, "z_difficulty": true, "sv_region": true, "sv_steamgroup": true}
	for name, value := range config.CVars {
		folded := strings.ToLower(name)
		if name != strings.TrimSpace(name) || !sourceIdentifier.MatchString(name) || isManagedSecretCVar(name) || reservedCVars[folded] || seenCVars[folded] {
			return fmt.Errorf("invalid or secret CVar %q", name)
		}
		seenCVars[folded] = true
		if err := validateManifestText("CVar "+name, value, 4096, true); err != nil {
			return err
		}
	}
	return nil
}

func validateManifestText(field, value string, maximum int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s contains invalid text", field)
	}
	return nil
}

func backupLaunchFromRecord(record InstanceRecord, instancePath string) (BackupLaunchConfig, error) {
	launch, err := parsePortableStartArgs(record.Args, record.RCONPort, record.TVPort)
	if err != nil {
		return BackupLaunchConfig{}, err
	}
	launch.AutoRestart = record.AutoRestart
	managed, err := readPortableManagedConfig(instancePath)
	if err != nil {
		return BackupLaunchConfig{}, err
	}
	launch.Hostname = managed.Hostname
	launch.Difficulty = managed.Difficulty
	launch.Region = managed.Region
	launch.SteamGroupID = managed.SteamGroupID
	launch.CVars = managed.CVars
	if err := validateBackupLaunchConfig(launch); err != nil {
		return BackupLaunchConfig{}, err
	}
	return launch, nil
}

func parsePortableStartArgs(args []string, fallbackPort, fallbackTVPort int) (BackupLaunchConfig, error) {
	if len(args) > 0 {
		if err := ValidateStartArgs(args); err != nil {
			return BackupLaunchConfig{}, fmt.Errorf("registered startup arguments: %w", err)
		}
	}
	port, tvPort := fallbackPort, fallbackTVPort
	if port == 0 {
		port = 27015
	}
	launch := BackupLaunchConfig{Mode: "coop", StartMap: "c1m1_hotel", MaxPlayers: 8}
	seen := make(map[string]bool)
	var parseErr error
	for index := 0; index < len(args); index++ {
		argument := args[index]
		name, inline, hasInline := splitStartArgument(argument)
		if name == "-console" {
			if seen[name] || (hasInline && inline != "") {
				return BackupLaunchConfig{}, errors.New("registered startup arguments contain invalid or duplicate -console")
			}
			seen[name] = true
			continue
		}
		managed := map[string]bool{"-game": true, "-port": true, "+hostport": true, "+tv_port": true, "+map": true, "+mp_gamemode": true, "+maxplayers": true, "+exec": true}
		if !managed[name] {
			launch.ExtraArgs = append(launch.ExtraArgs, argument)
			continue
		}
		canonical := name
		if name == "+hostport" {
			canonical = "-port"
		}
		if seen[canonical] {
			return BackupLaunchConfig{}, fmt.Errorf("registered startup arguments contain duplicate %s", canonical)
		}
		seen[canonical] = true
		value := inline
		if !hasInline {
			index++
			if index >= len(args) {
				return BackupLaunchConfig{}, fmt.Errorf("registered startup argument %s is missing its value", name)
			}
			value = args[index]
		}
		switch canonical {
		case "-game":
			if !strings.EqualFold(value, "left4dead2") {
				return BackupLaunchConfig{}, errors.New("registered startup arguments select another game")
			}
		case "-port":
			port, parseErr = strconv.Atoi(value)
		case "+tv_port":
			tvPort, parseErr = strconv.Atoi(value)
		case "+map":
			launch.StartMap = value
		case "+mp_gamemode":
			launch.Mode = value
		case "+maxplayers":
			launch.MaxPlayers, parseErr = strconv.Atoi(value)
		case "+exec":
			if !strings.EqualFold(value, "l4d2panel.cfg") {
				return BackupLaunchConfig{}, errors.New("registered startup arguments select an unmanaged config file")
			}
		}
		if parseErr != nil {
			return BackupLaunchConfig{}, fmt.Errorf("registered startup argument %s has an invalid integer", name)
		}
	}
	if port < 1 || port > 65535 || tvPort < 0 || tvPort > 65535 || (tvPort != 0 && port == tvPort) {
		return BackupLaunchConfig{}, errors.New("registered startup arguments contain invalid ports")
	}
	if fallbackPort != 0 && port != fallbackPort {
		return BackupLaunchConfig{}, errors.New("registered game port does not match the instance registry")
	}
	if fallbackTVPort != 0 && tvPort != fallbackTVPort {
		return BackupLaunchConfig{}, errors.New("registered SourceTV port does not match the instance registry")
	}
	if err := validateBackupLaunchConfig(launch); err != nil {
		return BackupLaunchConfig{}, err
	}
	return launch, nil
}

func readPortableManagedConfig(instancePath string) (BackupLaunchConfig, error) {
	raw, err := readRegularFileWithinNoFollow(instancePath, generatedConfigRelative, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return BackupLaunchConfig{}, nil
	}
	if err != nil {
		return BackupLaunchConfig{}, err
	}
	result := BackupLaunchConfig{CVars: make(map[string]string)}
	seen := make(map[string]bool)
	lines := bytes.Split(raw, []byte{'\n'})
	for index, rawLine := range lines {
		line := strings.TrimSuffix(string(rawLine), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, err := parseManagedConfigLine(line)
		if err != nil {
			return BackupLaunchConfig{}, fmt.Errorf("managed configuration line %d: %w", index+1, err)
		}
		foldedName := strings.ToLower(name)
		if seen[foldedName] {
			return BackupLaunchConfig{}, fmt.Errorf("managed configuration contains duplicate CVar %q", name)
		}
		seen[foldedName] = true
		if isManagedSecretCVar(name) {
			continue
		}
		switch strings.ToLower(name) {
		case "hostname":
			result.Hostname = value
		case "z_difficulty":
			result.Difficulty = value
		case "sv_region":
			result.Region = value
		case "sv_steamgroup":
			result.SteamGroupID = value
		default:
			result.CVars[name] = value
		}
	}
	if len(result.CVars) == 0 {
		result.CVars = nil
	}
	return result, nil
}

func parseManagedConfigLine(line string) (string, string, error) {
	space := strings.IndexAny(line, " \t")
	if space <= 0 {
		return "", "", errors.New("expected a quoted CVar assignment")
	}
	name := line[:space]
	if !sourceIdentifier.MatchString(name) {
		return "", "", errors.New("invalid CVar name")
	}
	rest := strings.TrimSpace(line[space:])
	if len(rest) < 2 || rest[0] != '"' {
		return "", "", errors.New("expected a quoted CVar value")
	}
	var value strings.Builder
	escaped := false
	closedAt := -1
	for index := 1; index < len(rest); index++ {
		character := rest[index]
		if escaped {
			if character != '\\' && character != '"' {
				return "", "", errors.New("invalid CVar escape")
			}
			value.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			closedAt = index
			break
		}
		value.WriteByte(character)
	}
	if escaped || closedAt < 0 || strings.TrimSpace(rest[closedAt+1:]) != "" {
		return "", "", errors.New("unterminated or trailing CVar value")
	}
	return name, value.String(), nil
}

func (config BackupLaunchConfig) domainConfig() domain.InstanceConfig {
	return domain.InstanceConfig{
		Hostname: config.Hostname, Mode: config.Mode, Difficulty: config.Difficulty,
		MaxPlayers: config.MaxPlayers, StartMap: config.StartMap, Region: config.Region,
		SteamGroupID: config.SteamGroupID, ExtraArgs: append([]string(nil), config.ExtraArgs...),
		CVars: cloneStringMap(config.CVars), AutoRestart: config.AutoRestart,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
