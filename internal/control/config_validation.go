package control

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

var sourceConfigIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validInstanceAgentHost(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "0.0.0.0", "127.0.0.1", "::", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func validateInstanceConfig(config domain.InstanceConfig) error {
	if config.MaxPlayers < 0 || config.MaxPlayers > 32 {
		return errors.New("max_players must be zero or between 1 and 32")
	}
	for field, value := range map[string]string{
		"hostname": config.Hostname, "region": config.Region, "steam_group_id": config.SteamGroupID,
	} {
		if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s contains invalid text", field)
		}
	}
	if err := validateServerPassword(config.ServerPassword); err != nil {
		return err
	}
	if config.Mode != "" && !sourceConfigIdentifier.MatchString(config.Mode) {
		return errors.New("mode may only contain letters, numbers, dash, and underscore")
	}
	if config.StartMap != "" && !sourceConfigIdentifier.MatchString(config.StartMap) {
		return errors.New("start_map may only contain letters, numbers, dash, and underscore")
	}
	if config.Difficulty != "" {
		switch strings.ToLower(config.Difficulty) {
		case "easy", "normal", "hard", "impossible":
		default:
			return errors.New("difficulty must be easy, normal, hard, or impossible")
		}
	}
	if config.SteamGroupID != "" {
		if len(config.SteamGroupID) > 20 || strings.Trim(config.SteamGroupID, "0123456789") != "" {
			return errors.New("steam_group_id must contain at most 20 decimal digits")
		}
	}
	if len(config.MaintenanceCron) > 100 || strings.ContainsAny(config.MaintenanceCron, "\x00\r\n") {
		return errors.New("maintenance_cron contains invalid text")
	}
	if config.MaintenanceCron != "" {
		if _, err := parseFiveFieldCron(config.MaintenanceCron); err != nil {
			return fmt.Errorf("maintenance_cron must be a valid standard five-field cron expression: %w", err)
		}
	}
	if err := validateConfigExtraArgs(config.ExtraArgs); err != nil {
		return err
	}
	if len(config.CVars) > 256 {
		return errors.New("configuration contains too many custom CVars")
	}
	for name, value := range config.CVars {
		trimmedName := strings.TrimSpace(name)
		if name != trimmedName || !sourceConfigIdentifier.MatchString(name) {
			return fmt.Errorf("invalid CVar name %q", name)
		}
		switch strings.ToLower(trimmedName) {
		case "rcon_password", "sv_password":
			return fmt.Errorf("CVar %q is managed by the encrypted secret store", name)
		}
		if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("CVar %q contains invalid text", name)
		}
	}
	return nil
}

func validateServerPassword(value string) error {
	if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("server_password contains invalid text")
	}
	return nil
}

func validateConfigExtraArgs(args []string) error {
	if len(args) > 128 {
		return errors.New("too many advanced startup arguments")
	}
	if err := validateTickrateArguments(args); err != nil {
		return err
	}
	reserved := map[string]bool{
		"-console": true, "-game": true, "-port": true, "+hostport": true,
		"+map": true, "+mp_gamemode": true, "+maxplayers": true, "+exec": true,
		"+tv_port": true, "+rcon_password": true, "+sv_password": true, "+plugin_load": true,
	}
	for index, argument := range args {
		if strings.TrimSpace(argument) == "" || len(argument) > 4096 || strings.ContainsAny(argument, "\x00\r\n;") {
			return fmt.Errorf("advanced argument %d contains invalid text", index)
		}
		name := configArgumentName(argument)
		if reserved[name] {
			return fmt.Errorf("advanced argument %q conflicts with a managed setting", argument)
		}
		if name == "-basedir" || name == "-defaultgamedir" {
			return fmt.Errorf("unsafe startup parameter %q", argument)
		}
	}
	return nil
}

func validateTickrateArguments(args []string) error {
	seen := false
	for index, argument := range args {
		if configArgumentName(argument) != "-tickrate" {
			continue
		}
		if seen {
			return errors.New("-tickrate may only be supplied once")
		}
		seen = true
		value := ""
		trimmed := strings.TrimSpace(argument)
		if separator := strings.IndexFunc(trimmed, func(r rune) bool { return r == '=' || unicode.IsSpace(r) }); separator >= 0 {
			value = strings.TrimLeftFunc(trimmed[separator:], func(r rune) bool { return r == '=' || unicode.IsSpace(r) })
		} else if index+1 < len(args) {
			value = strings.TrimSpace(args[index+1])
		}
		tickrate, err := strconv.Atoi(value)
		if err != nil || tickrate < 1 || tickrate > 1000 {
			return errors.New("-tickrate must be followed by an integer between 1 and 1000")
		}
	}
	return nil
}

// configArgumentName returns the first Source/Steam command-line token. Both
// '=' and whitespace can separate an option from a value inside one argv
// element, so matching the complete string would let managed parameters bypass
// validation.
func configArgumentName(argument string) string {
	argument = strings.TrimSpace(argument)
	if separator := strings.IndexFunc(argument, func(r rune) bool {
		return r == '=' || unicode.IsSpace(r)
	}); separator >= 0 {
		argument = argument[:separator]
	}
	return strings.ToLower(argument)
}
