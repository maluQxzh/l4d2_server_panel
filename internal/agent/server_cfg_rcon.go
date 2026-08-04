package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const serverConfigRelative = "left4dead2/cfg/server.cfg"

func (e *Executor) executeInstanceRCON(ctx context.Context, record InstanceRecord, payload operationPayload, password, command string, timeout time.Duration) (string, error) {
	response, err := executeLocalRCON(ctx, record, payload, password, command, timeout)
	if err == nil || !isRCONDialError(err) {
		return response, err
	}
	return "", fmt.Errorf("%w; %s", err, e.rconConfigurationDiagnosis(record, password))
}

func (e *Executor) rconConfigurationDiagnosis(record InstanceRecord, expectedPassword string) string {
	for _, argument := range record.Args {
		name, _, _ := splitStartArgument(argument)
		if name == "-usetcp" {
			return "启动参数 -usetcp 会关闭 Source TCP 支持，因此 RCON 无法监听"
		}
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return "Agent 无法解析实例路径以检查 left4dead2/cfg/server.cfg"
	}
	raw, err := readRegularFileWithinNoFollow(instancePath, serverConfigRelative, maxManagedFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return "left4dead2/cfg/server.cfg 不存在，SRCDS 无法从中加载 rcon_password"
	}
	if err != nil {
		return "Agent 无法安全读取 left4dead2/cfg/server.cfg"
	}
	configuredPassword, found, parseErr := effectiveServerConfigRCONPassword(raw)
	if parseErr != nil {
		return "left4dead2/cfg/server.cfg 不是有效 UTF-8 文本，或包含未闭合的引号"
	}
	if !found {
		return "left4dead2/cfg/server.cfg 中没有生效的 rcon_password 指令"
	}
	if configuredPassword == "" {
		return "left4dead2/cfg/server.cfg 将 rcon_password 设为空值，因此 RCON 已关闭"
	}
	if !constantTimeStringEqual(configuredPassword, expectedPassword) {
		return "left4dead2/cfg/server.cfg 中最终生效的 rcon_password 与中心端保存的凭据不一致"
	}
	return "server.cfg 中存在与中心端一致的非空 rcon_password，但当前 SRCDS 仍未打开游戏端口的 TCP RCON；请确认已使用新版启动参数重启，并检查控制台中 server.cfg 执行失败或后续覆盖密码的记录"
}

func constantTimeStringEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func effectiveServerConfigRCONPassword(raw []byte) (string, bool, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", false, errors.New("server.cfg must be UTF-8 text")
	}
	var (
		command  []string
		token    strings.Builder
		tokenSet bool
		quoted   bool
		escaped  bool
		password string
		found    bool
	)
	flushToken := func() {
		if !tokenSet {
			return
		}
		command = append(command, token.String())
		token.Reset()
		tokenSet = false
	}
	flushCommand := func() {
		flushToken()
		if len(command) >= 2 && strings.EqualFold(command[0], "rcon_password") {
			password, found = command[1], true
		}
		command = command[:0]
	}
	for index := 0; index < len(raw); index++ {
		character := raw[index]
		if quoted {
			if escaped {
				if character != '"' && character != '\\' {
					token.WriteByte('\\')
				}
				token.WriteByte(character)
				tokenSet = true
				escaped = false
				continue
			}
			switch character {
			case '\\':
				escaped = true
			case '"':
				quoted = false
			default:
				token.WriteByte(character)
				tokenSet = true
			}
			continue
		}
		if character == '/' && index+1 < len(raw) && raw[index+1] == '/' {
			flushCommand()
			for index+1 < len(raw) && raw[index+1] != '\n' {
				index++
			}
			continue
		}
		switch character {
		case '"':
			quoted = true
			tokenSet = true
		case ' ', '\t', '\r':
			flushToken()
		case '\n', ';':
			flushCommand()
		default:
			token.WriteByte(character)
			tokenSet = true
		}
	}
	if quoted || escaped {
		return "", false, errors.New("unterminated quoted value")
	}
	flushCommand()
	return password, found, nil
}
