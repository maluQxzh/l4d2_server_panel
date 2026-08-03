package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type StartOptions struct {
	Port       int
	TVPort     int
	MaxPlayers int
	Map        string
	Mode       string
	ConfigFile string
	ExtraArgs  []string
}

var sourceIdentifier = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

func BuildStartArgs(options StartOptions) ([]string, error) {
	if options.Port <= 0 || options.Port > 65535 {
		return nil, errors.New("game port must be between 1 and 65535")
	}
	if options.TVPort < 0 || options.TVPort > 65535 || (options.TVPort != 0 && options.TVPort == options.Port) {
		return nil, errors.New("invalid SourceTV port")
	}
	if options.MaxPlayers <= 0 || options.MaxPlayers > 32 {
		return nil, errors.New("max players must be between 1 and 32")
	}
	if options.Map == "" {
		options.Map = "c1m1_hotel"
	}
	if options.Mode == "" {
		options.Mode = "coop"
	}
	if !sourceIdentifier.MatchString(options.Map) || !sourceIdentifier.MatchString(options.Mode) {
		return nil, errors.New("map and mode may only contain letters, numbers, dash, and underscore")
	}
	if options.ConfigFile == "" {
		options.ConfigFile = "l4d2panel.cfg"
	}
	if strings.ContainsAny(options.ConfigFile, `/\\`) || !strings.HasSuffix(strings.ToLower(options.ConfigFile), ".cfg") {
		return nil, errors.New("config file must be a cfg filename")
	}
	if err := validateExtraStartArgs(options.ExtraArgs); err != nil {
		return nil, err
	}
	args := []string{"-console", "-game", "left4dead2", "-port", strconv.Itoa(options.Port)}
	if options.TVPort != 0 {
		args = append(args, "+tv_port", strconv.Itoa(options.TVPort))
	}
	args = append(args, "+map", options.Map, "+mp_gamemode", options.Mode, "+maxplayers", strconv.Itoa(options.MaxPlayers), "+exec", options.ConfigFile)
	return append(args, options.ExtraArgs...), nil
}

func ValidateStartArgs(args []string) error {
	if len(args) == 0 || len(args) > 256 {
		return errors.New("startup arguments must contain between 1 and 256 elements")
	}
	if err := validateTickrateArguments(args); err != nil {
		return err
	}
	for i, arg := range args {
		if err := validateArgumentText(arg); err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
		name, inlineValue, hasInlineValue := splitStartArgument(arg)
		if name == "-basedir" || name == "-defaultgamedir" {
			return fmt.Errorf("unsafe startup parameter %q", arg)
		}
		if name == "+rcon_password" {
			return errors.New("RCON password must only be supplied through generated configuration")
		}
		if name == "+sv_password" {
			return errors.New("server password must only be supplied through generated configuration")
		}
		if name == "+plugin_load" {
			return errors.New("native plugins must only be installed through the package manager")
		}
		if name == "-game" {
			game := inlineValue
			if !hasInlineValue && i+1 < len(args) {
				game = args[i+1]
			}
			if !strings.EqualFold(strings.TrimSpace(game), "left4dead2") {
				return errors.New("-game may only select left4dead2")
			}
		}
		if name == "+exec" {
			configFile := inlineValue
			if !hasInlineValue && i+1 < len(args) {
				configFile = args[i+1]
			}
			if !strings.EqualFold(strings.TrimSpace(configFile), "l4d2panel.cfg") {
				return errors.New("+exec may only select l4d2panel.cfg")
			}
		}
	}
	return nil
}

func validateExtraStartArgs(args []string) error {
	if len(args) > 128 {
		return errors.New("too many advanced startup arguments")
	}
	if err := validateTickrateArguments(args); err != nil {
		return err
	}
	reserved := map[string]bool{"-console": true, "-game": true, "-port": true, "+hostport": true, "+map": true, "+mp_gamemode": true, "+maxplayers": true, "+exec": true, "+tv_port": true, "+rcon_password": true, "+sv_password": true, "+plugin_load": true}
	for i, arg := range args {
		if err := validateArgumentText(arg); err != nil {
			return fmt.Errorf("advanced argument %d: %w", i, err)
		}
		name, _, _ := splitStartArgument(arg)
		if reserved[name] {
			return fmt.Errorf("advanced argument %q conflicts with a managed setting", arg)
		}
		if name == "-basedir" || name == "-defaultgamedir" {
			return fmt.Errorf("unsafe startup parameter %q", arg)
		}
	}
	return nil
}

func validateTickrateArguments(args []string) error {
	seen := false
	for index, argument := range args {
		name, inlineValue, hasInlineValue := splitStartArgument(argument)
		if name != "-tickrate" {
			continue
		}
		if seen {
			return errors.New("-tickrate may only be supplied once")
		}
		seen = true
		value := inlineValue
		if !hasInlineValue && index+1 < len(args) {
			value = strings.TrimSpace(args[index+1])
		}
		tickrate, err := strconv.Atoi(value)
		if err != nil || tickrate < 1 || tickrate > 1000 {
			return errors.New("-tickrate must be followed by an integer between 1 and 1000")
		}
	}
	return nil
}

func validateArgumentText(arg string) error {
	if strings.TrimSpace(arg) == "" {
		return errors.New("argument is empty")
	}
	if len(arg) > 4096 {
		return errors.New("argument is too long")
	}
	if strings.ContainsAny(arg, "\x00\r\n;") {
		return errors.New("argument contains a forbidden control or command separator")
	}
	return nil
}

// splitStartArgument identifies the Source/Steam first token even when a
// caller packs a parameter and its value into one argv element. Source command
// line parameters commonly accept both "name=value" and "name value" forms,
// so security decisions must not compare the whole argv string.
func splitStartArgument(argument string) (name, inlineValue string, hasInlineValue bool) {
	argument = strings.TrimSpace(argument)
	separator := strings.IndexFunc(argument, func(r rune) bool {
		return r == '=' || unicode.IsSpace(r)
	})
	if separator < 0 {
		return strings.ToLower(argument), "", false
	}
	name = strings.ToLower(argument[:separator])
	inlineValue = strings.TrimLeftFunc(argument[separator:], func(r rune) bool {
		return r == '=' || unicode.IsSpace(r)
	})
	return name, inlineValue, true
}
