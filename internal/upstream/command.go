// Package upstream understands the upstream frp-panel client command line.
//
// It exists because the frp-panel master WebUI hands out a ready to paste
// start command such as:
//
//	./frp-panel client -s <secret> -i user.c.client1 --api-url http://host:9000 --rpc-url grpc://host:9001
//
// Parsing that string lets the user configure this app by copy and paste.
// Nothing in this package executes anything: the string is tokenized in-process
// (never handed to a shell), shell syntax is rejected outright, and the child
// process argument vector is rebuilt from validated structured fields.
package upstream

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Result is the outcome of parsing a pasted command.
type Result struct {
	ClientID    string `json:"client_id"`
	Secret      string `json:"-"`
	APIURL      string `json:"api_url"`
	RPCURL      string `json:"rpc_url"`
	ProgramHint string `json:"program_hint"`
	// SecretDetectedField and SecretLength describe the secret without
	// revealing any part of it, so the result can be returned to the browser.
	SecretDetectedField bool     `json:"secret_detected"`
	SecretLength        int      `json:"secret_length"`
	Warnings            []string `json:"warnings"`
	Missing             []string `json:"missing"`
}

// SecretDetected reports whether a secret was found without exposing it.
func (r Result) SecretDetected() bool { return r.Secret != "" }

// LaunchSpec is the validated input used to build the child process argv/env.
type LaunchSpec struct {
	ClientID          string
	Secret            string
	APIURL            string
	RPCURL            string
	LogLevel          string
	EnableRemoteShell bool
	EnableFunctions   bool
}

// Argv builds the argument vector. Only structured, validated values are used,
// so no raw user token can ever be interpreted as an option.
func (l LaunchSpec) Argv() []string {
	return []string{
		"client",
		"--id=" + l.ClientID,
		"--api-url=" + l.APIURL,
		"--rpc-url=" + l.RPCURL,
	}
}

// Env builds the environment additions.
//
// The secret travels in the environment rather than in argv so that it does not
// show up in `ps` output or in the global process list. Feature switches are
// always set explicitly: the upstream defaults for remote shell and functions
// are true, and this app must not enable them implicitly.
func (l LaunchSpec) Env() []string {
	level := l.LogLevel
	if level == "" {
		level = "info"
	}
	return []string{
		"CLIENT_ID=" + l.ClientID,
		"CLIENT_SECRET=" + l.Secret,
		"CLIENT_API_URL=" + l.APIURL,
		"CLIENT_RPC_URL=" + l.RPCURL,
		"LOGGER_DEFAULT_LOGGER_LEVEL=" + level,
		"LOGGER_FRP_LOGGER_LEVEL=" + level,
		"CLIENT_FEATURES_ENABLE_REMOTE_SHELL=" + strconv.FormatBool(l.EnableRemoteShell),
		"CLIENT_FEATURES_ENABLE_FUNCTIONS=" + strconv.FormatBool(l.EnableFunctions),
	}
}

type flagKind int

const (
	kindString flagKind = iota
	kindBool
)

type flagSpec struct {
	long  string
	short string
	kind  flagKind
}

var (
	flagSecret   = flagSpec{long: "secret", short: "s", kind: kindString}
	flagID       = flagSpec{long: "id", short: "i", kind: kindString}
	flagAPIURL   = flagSpec{long: "api-url", kind: kindString}
	flagRPCURL   = flagSpec{long: "rpc-url", kind: kindString}
	flagJoinToke = flagSpec{long: "join-token", short: "j", kind: kindString}
	flagApp      = flagSpec{long: "app", short: "a", kind: kindString}
	flagEphemer  = flagSpec{long: "ephemeral", kind: kindBool}

	flagRPCHost   = flagSpec{long: "rpc-host", short: "r", kind: kindString}
	flagAPIHost   = flagSpec{long: "api-host", short: "t", kind: kindString}
	flagRPCPort   = flagSpec{long: "rpc-port", short: "c", kind: kindString}
	flagAPIPort   = flagSpec{long: "api-port", short: "p", kind: kindString}
	flagAPIScheme = flagSpec{long: "api-scheme", short: "e", kind: kindString}
)

var knownFlags = []flagSpec{
	flagSecret, flagID, flagAPIURL, flagRPCURL, flagJoinToke, flagApp, flagEphemer,
	flagRPCHost, flagAPIHost, flagRPCPort, flagAPIPort, flagAPIScheme,
}

// allowedEnvAssignments is the whitelist of leading NAME=VALUE tokens accepted
// in a pasted command.
var allowedEnvAssignments = map[string]bool{
	"CLIENT_ID":                           true,
	"CLIENT_SECRET":                       true,
	"CLIENT_API_URL":                      true,
	"CLIENT_RPC_URL":                      true,
	"LOGGER_DEFAULT_LOGGER_LEVEL":         true,
	"LOGGER_FRP_LOGGER_LEVEL":             true,
	"CLIENT_FEATURES_ENABLE_REMOTE_SHELL": true,
	"CLIENT_FEATURES_ENABLE_FUNCTIONS":    true,
}

var programNames = map[string]bool{
	"frp-panel":        true,
	"frp-panel-client": true,
	"frp-panel.exe":    true,
	"frpp":             true,
	"frppc":            true,
	"frpp.exe":         true,
	"frppc.exe":        true,
	"frpc":             true,
}

// ErrShellSyntax is returned when the pasted text is a shell command rather
// than a plain argument list.
var ErrShellSyntax = errors.New("检测到 shell 语法（管道、重定向、命令替换或后台符），本应用不会执行 shell 命令")

// Tokenize splits a command line into arguments using POSIX-like quoting rules.
// It performs no expansion of any kind and rejects unquoted shell operators.
func Tokenize(line string) ([]string, error) {
	var (
		tokens  []string
		cur     strings.Builder
		started bool
		i       int
	)
	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i < len(line) {
		c := line[i]
		switch c {
		case ' ', '\t', '\r', '\n':
			flush()
			i++
		case '#':
			if !started {
				// Comment to end of line.
				return tokens, nil
			}
			cur.WriteByte(c)
			started = true
			i++
		case '\'':
			started = true
			i++
			for i < len(line) && line[i] != '\'' {
				cur.WriteByte(line[i])
				i++
			}
			if i >= len(line) {
				return nil, errors.New("引号未闭合：单引号缺少结束符")
			}
			i++
		case '"':
			started = true
			i++
			closed := false
			for i < len(line) {
				ch := line[i]
				if ch == '\\' && i+1 < len(line) {
					next := line[i+1]
					switch next {
					case '"', '\\', '$', '`':
						cur.WriteByte(next)
						i += 2
						continue
					case '\n':
						i += 2
						continue
					default:
						cur.WriteByte(ch)
						i++
						continue
					}
				}
				if ch == '"' {
					closed = true
					i++
					break
				}
				cur.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, errors.New("引号未闭合：双引号缺少结束符")
			}
		case '\\':
			if i+1 >= len(line) {
				return nil, errors.New("命令以孤立的反斜杠结尾")
			}
			// Line continuation: "\<newline>" (and "\<CRLF>") joins the lines
			// without producing a character.
			if line[i+1] == '\n' {
				i += 2
				continue
			}
			if line[i+1] == '\r' && i+2 < len(line) && line[i+2] == '\n' {
				i += 3
				continue
			}
			started = true
			cur.WriteByte(line[i+1])
			i += 2
		case '|', '&', ';', '>', '<', '`':
			return nil, ErrShellSyntax
		case '$':
			if i+1 < len(line) && (line[i+1] == '(' || line[i+1] == '{') {
				return nil, ErrShellSyntax
			}
			cur.WriteByte(c)
			started = true
			i++
		default:
			started = true
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return tokens, nil
}

// ParseCommand parses a pasted frp-panel client command.
func ParseCommand(line string) (Result, error) {
	res := Result{}
	if strings.TrimSpace(line) == "" {
		return res, errors.New("请先粘贴 frp-panel 的客户端启动命令")
	}
	tokens, err := Tokenize(line)
	if err != nil {
		return res, err
	}
	if len(tokens) == 0 {
		return res, errors.New("命令为空或全部为注释")
	}

	idx := 0
	for idx < len(tokens) && tokens[idx] == "sudo" {
		idx++
	}

	var (
		apiScheme, apiHost, rpcHost string
		apiPort, rpcPort            int
		seenDeprecated              bool
	)
	for idx < len(tokens) {
		tok := tokens[idx]
		name, value, ok := splitEnvAssignment(tok)
		if !ok {
			break
		}
		if !allowedEnvAssignments[name] {
			return res, fmt.Errorf("不支持的环境变量 %s，请改用对应的命令行参数", name)
		}
		if applyValue(&res, name, value) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("从环境变量赋值读取了 %s", name))
		} else {
			// 日志级别与功能开关是有意不从这里生效的：前者在界面上有下拉框，
			// 后者涉及安全边界（远程命令执行 / Serverless 函数），必须由使用者
			// 在本应用的界面上显式勾选，绝不能因为粘贴了一行命令就被打开。
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("已忽略环境变量赋值 %s=%s：请在管理界面里通过对应开关/下拉框设置", name, value))
		}
		idx++
	}

	if idx < len(tokens) && looksLikeProgram(tokens[idx]) {
		res.ProgramHint = tokens[idx]
		idx++
	}

	if idx < len(tokens) {
		switch tokens[idx] {
		case "client":
			idx++
		case "master", "server":
			return res, fmt.Errorf("这是 %q 角色的启动命令，本应用只管理 client", tokens[idx])
		}
	}

	for idx < len(tokens) {
		tok := tokens[idx]
		if tok == "" {
			idx++
			continue
		}
		if !strings.HasPrefix(tok, "-") {
			return res, fmt.Errorf("无法识别的参数 %q，请只粘贴 frp-panel client 的启动参数", tok)
		}
		spec, value, hasValue, err := resolveFlag(tok)
		if err != nil {
			return res, err
		}
		if spec.kind == kindString && !hasValue {
			if idx+1 >= len(tokens) {
				return res, fmt.Errorf("参数 %s 缺少取值", tok)
			}
			value = tokens[idx+1]
			hasValue = true
			idx++
		}
		idx++
		if spec.kind == kindBool && !hasValue {
			value = "true"
		}

		switch spec.long {
		case flagSecret.long:
			res.Secret = value
		case flagID.long:
			res.ClientID = value
		case flagAPIURL.long:
			res.APIURL = value
		case flagRPCURL.long:
			res.RPCURL = value
		case flagJoinToke.long:
			return res, errors.New("本应用第一版不支持 join-token 自动加入：请在 Master 的 WebUI 中创建客户端，" +
				"然后复制包含 -s / -i 的启动命令")
		case flagApp.long:
			return res, errors.New("参数 -a/--app 已被上游移除，请复制不含该参数的新启动命令")
		case flagEphemer.long:
			res.Warnings = append(res.Warnings, "已忽略 --ephemeral（仅在 join-token 流程中使用）")
		case flagRPCHost.long:
			rpcHost = value
			seenDeprecated = true
		case flagAPIHost.long:
			apiHost = value
			seenDeprecated = true
		case flagRPCPort.long:
			n, err := strconv.Atoi(value)
			if err != nil {
				return res, fmt.Errorf("--rpc-port 取值 %q 不是数字", value)
			}
			rpcPort = n
			seenDeprecated = true
		case flagAPIPort.long:
			n, err := strconv.Atoi(value)
			if err != nil {
				return res, fmt.Errorf("--api-port 取值 %q 不是数字", value)
			}
			apiPort = n
			seenDeprecated = true
		case flagAPIScheme.long:
			apiScheme = value
			seenDeprecated = true
		}
	}

	if seenDeprecated {
		if res.APIURL == "" && apiHost != "" {
			scheme := apiScheme
			if scheme == "" {
				scheme = "http"
			}
			port := apiPort
			if port == 0 {
				port = 9000
			}
			res.APIURL = fmt.Sprintf("%s://%s:%d", scheme, apiHost, port)
			res.Warnings = append(res.Warnings,
				"API URL 由已废弃的 --api-host/--api-port/--api-scheme 推导，请核对端口与协议")
		}
		if res.RPCURL == "" && rpcHost != "" {
			port := rpcPort
			if port == 0 {
				port = 9001
			}
			res.RPCURL = fmt.Sprintf("grpc://%s:%d", rpcHost, port)
			res.Warnings = append(res.Warnings,
				"RPC URL 由已废弃的 --rpc-host/--rpc-port 推导为 grpc://，请核对（如需 TLS 反代请改为 wss://）")
		}
		if res.APIURL == "" || res.RPCURL == "" {
			res.Warnings = append(res.Warnings,
				"检测到已废弃参数，但缺少 --api-host 或 --rpc-host，无法推导完整地址")
		}
	}

	if res.ClientID == "" {
		res.Missing = append(res.Missing, "客户端 ID")
	}
	if res.Secret == "" {
		res.Missing = append(res.Missing, "客户端密钥")
	}
	if res.APIURL == "" {
		res.Missing = append(res.Missing, "API URL")
	}
	if res.RPCURL == "" {
		res.Missing = append(res.Missing, "RPC URL")
	}
	res.SecretDetectedField = res.Secret != ""
	res.SecretLength = len([]rune(res.Secret))
	return res, nil
}

// applyValue maps a whitelisted environment assignment onto a Result field and
// reports whether the assignment was actually applied.
func applyValue(res *Result, name, value string) bool {
	switch name {
	case "CLIENT_ID":
		res.ClientID = value
	case "CLIENT_SECRET":
		res.Secret = value
	case "CLIENT_API_URL":
		res.APIURL = value
	case "CLIENT_RPC_URL":
		res.RPCURL = value
	default:
		return false
	}
	return true
}

func splitEnvAssignment(tok string) (name, value string, ok bool) {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return "", "", false
	}
	name = tok[:eq]
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return "", "", false
		}
	}
	return name, tok[eq+1:], true
}

func looksLikeProgram(tok string) bool {
	if tok == "" {
		return false
	}
	if strings.ContainsAny(tok, `/\`) {
		return true
	}
	return programNames[strings.ToLower(tok)]
}

func resolveFlag(tok string) (flagSpec, string, bool, error) {
	var (
		name     string
		value    string
		hasValue bool
	)
	if strings.HasPrefix(tok, "--") {
		body := tok[2:]
		if eq := strings.IndexByte(body, '='); eq >= 0 {
			name, value, hasValue = body[:eq], body[eq+1:], true
		} else {
			name = body
		}
	} else {
		body := tok[1:]
		if body == "" {
			return flagSpec{}, "", false, errors.New("无法识别的参数 \"-\"")
		}
		name = body[:1]
		rest := body[1:]
		if rest != "" {
			if strings.HasPrefix(rest, "=") {
				value, hasValue = rest[1:], true
			} else {
				value, hasValue = rest, true
			}
		}
	}
	for _, spec := range knownFlags {
		if name == spec.long || (len(name) == 1 && name == spec.short) {
			return spec, value, hasValue, nil
		}
	}
	return flagSpec{}, "", false, fmt.Errorf("无法识别的参数 %s，本应用只支持 -s/-i/--api-url/--rpc-url", tok)
}
