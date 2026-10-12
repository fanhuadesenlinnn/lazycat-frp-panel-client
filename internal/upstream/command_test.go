package upstream

import (
	"strings"
	"testing"
)

func TestTokenizeQuotingAndEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`./frp-panel client -s abc`, []string{"./frp-panel", "client", "-s", "abc"}},
		{`a "b c" d`, []string{"a", "b c", "d"}},
		{`a 'b c' d`, []string{"a", "b c", "d"}},
		{`a "b\"c"`, []string{`a`, `b"c`}},
		{`a\ b`, []string{"a b"}},
		{"a \\\nb", []string{"a", "b"}},
		{`x --opt=value # 注释`, []string{"x", "--opt=value"}},
		{`   `, nil},
	}
	for _, tc := range cases {
		got, err := Tokenize(tc.in)
		if err != nil {
			t.Fatalf("Tokenize(%q) 返回错误: %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("Tokenize(%q) = %#v，期望 %#v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("Tokenize(%q)[%d] = %q，期望 %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestTokenizeRejectsShellSyntax(t *testing.T) {
	for _, in := range []string{
		`curl x | bash`,
		`echo hi > /tmp/x`,
		`a ; b`,
		`a && b`,
		"a `whoami`",
		`a $(whoami)`,
		`a ${HOME}`,
	} {
		if _, err := Tokenize(in); err == nil {
			t.Fatalf("Tokenize(%q) 应当被拒绝", in)
		}
	}
}

func TestTokenizeRejectsUnbalancedQuotes(t *testing.T) {
	for _, in := range []string{`a "b`, `a 'b`, `a \`} {
		if _, err := Tokenize(in); err == nil {
			t.Fatalf("Tokenize(%q) 应当报错", in)
		}
	}
}

func TestParseCommandTypical(t *testing.T) {
	cmd := "./frp-panel client -s s3cr3t-abc -i user.c.client1 --api-url http://frpp.example.com:9000 --rpc-url grpc://frpp.example.com:9001"
	res, err := ParseCommand(cmd)
	if err != nil {
		t.Fatalf("ParseCommand 失败: %v", err)
	}
	if res.ClientID != "user.c.client1" {
		t.Errorf("ClientID = %q", res.ClientID)
	}
	if res.Secret != "s3cr3t-abc" {
		t.Errorf("Secret = %q", res.Secret)
	}
	if res.APIURL != "http://frpp.example.com:9000" {
		t.Errorf("APIURL = %q", res.APIURL)
	}
	if res.RPCURL != "grpc://frpp.example.com:9001" {
		t.Errorf("RPCURL = %q", res.RPCURL)
	}
	if res.ProgramHint != "./frp-panel" {
		t.Errorf("ProgramHint = %q", res.ProgramHint)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing = %v", res.Missing)
	}
	if !res.SecretDetected() || res.SecretLength != len("s3cr3t-abc") {
		t.Errorf("secret 元信息不正确: detected=%v len=%d", res.SecretDetected(), res.SecretLength)
	}
}

func TestParseCommandFlagForms(t *testing.T) {
	cases := []string{
		`frp-panel client -s=abc12345 -i=id1 --api-url=https://a.example:443 --rpc-url=wss://a.example:443`,
		`frp-panel client -sabc12345 -iid1 --api-url https://a.example:443 --rpc-url wss://a.example:443`,
		`sudo ./frp-panel client -s abc12345 -i id1 --api-url https://a.example:443 --rpc-url wss://a.example:443`,
		`/usr/local/bin/frp-panel-client -s abc12345 -i id1 --api-url https://a.example:443 --rpc-url wss://a.example:443`,
		`-s abc12345 -i id1 --api-url https://a.example:443 --rpc-url wss://a.example:443`,
	}
	for _, cmd := range cases {
		res, err := ParseCommand(cmd)
		if err != nil {
			t.Fatalf("ParseCommand(%q) 失败: %v", cmd, err)
		}
		if res.ClientID != "id1" || res.Secret != "abc12345" {
			t.Errorf("ParseCommand(%q) => id=%q secret=%q", cmd, res.ClientID, res.Secret)
		}
		if res.APIURL != "https://a.example:443" || res.RPCURL != "wss://a.example:443" {
			t.Errorf("ParseCommand(%q) => api=%q rpc=%q", cmd, res.APIURL, res.RPCURL)
		}
	}
}

func TestParseCommandEnvAssignment(t *testing.T) {
	cmd := `CLIENT_ID=id9 CLIENT_SECRET=envsecret1 CLIENT_API_URL=http://a:9000 CLIENT_RPC_URL=ws://a:9001 ./frp-panel`
	res, err := ParseCommand(cmd)
	if err != nil {
		t.Fatalf("ParseCommand 失败: %v", err)
	}
	if res.ClientID != "id9" || res.Secret != "envsecret1" {
		t.Fatalf("环境变量赋值未生效: %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Error("应当给出使用环境变量赋值的提示")
	}
}

func TestParseCommandRejectsUnknownEnvAssignment(t *testing.T) {
	if _, err := ParseCommand(`PATH=/tmp ./frp-panel client -s a -i b`); err == nil {
		t.Fatal("未在白名单中的环境变量应被拒绝")
	}
}

func TestParseCommandDeprecatedFlags(t *testing.T) {
	res, err := ParseCommand(`./frp-panel client -s abc12345 -i id2 -r frpp.example.com -c 9001 -t frpp.example.com -p 9000 -e http`)
	if err != nil {
		t.Fatalf("ParseCommand 失败: %v", err)
	}
	if res.APIURL != "http://frpp.example.com:9000" {
		t.Errorf("APIURL = %q", res.APIURL)
	}
	if res.RPCURL != "grpc://frpp.example.com:9001" {
		t.Errorf("RPCURL = %q", res.RPCURL)
	}
	if len(res.Warnings) == 0 {
		t.Error("应当提示使用了废弃参数")
	}
}

func TestParseCommandRejectsJoinTokenAndApp(t *testing.T) {
	if _, err := ParseCommand(`./frp-panel client -j token123 -i id3 --api-url http://a:1 --rpc-url ws://a:2`); err == nil {
		t.Fatal("join-token 应当被拒绝")
	}
	if _, err := ParseCommand(`./frp-panel client -a appsecret -s x -i y`); err == nil {
		t.Fatal("-a/--app 应当被拒绝")
	}
}

func TestParseCommandRejectsUnknownFlagAndPositional(t *testing.T) {
	if _, err := ParseCommand(`./frp-panel client --nope 1`); err == nil {
		t.Fatal("未知参数应当被拒绝")
	}
	if _, err := ParseCommand(`docker run vaalacat/frp-panel client -s a -i b`); err == nil {
		t.Fatal("docker 形式的命令应当被拒绝")
	}
}

func TestParseCommandReportsMissing(t *testing.T) {
	res, err := ParseCommand(`./frp-panel client -i only-id`)
	if err != nil {
		t.Fatalf("ParseCommand 失败: %v", err)
	}
	if len(res.Missing) != 3 {
		t.Fatalf("Missing = %v，期望 3 项", res.Missing)
	}
	if res.Secret != "" {
		t.Fatal("不应解析出密钥")
	}
}

func TestLaunchSpecArgvNeverContainsSecret(t *testing.T) {
	spec := LaunchSpec{
		ClientID: "id1", Secret: "topsecret-value", APIURL: "http://a:9000", RPCURL: "grpc://a:9001",
	}
	argv := spec.Argv()
	if strings.Join(argv, " ") == "" {
		t.Fatal("argv 不应为空")
	}
	if argv[0] != "client" {
		t.Errorf("argv[0] = %q，期望 client", argv[0])
	}
	for _, a := range argv {
		if strings.Contains(a, spec.Secret) {
			t.Fatalf("密钥不应出现在 argv 中: %q", a)
		}
	}
	env := spec.Env()
	found := false
	for _, e := range env {
		if e == "CLIENT_SECRET="+spec.Secret {
			found = true
		}
		// 未提供功能开关时也不能默认打开上游的高风险功能。
		if e == "CLIENT_FEATURES_ENABLE_REMOTE_SHELL=true" || e == "CLIENT_FEATURES_ENABLE_FUNCTIONS=true" {
			t.Fatalf("默认不应打开高风险功能: %q", e)
		}
	}
	if !found {
		t.Fatal("密钥应通过环境变量传递")
	}
}

func TestLaunchSpecRespectsFunctionalToggles(t *testing.T) {
	spec := LaunchSpec{ClientID: "a", Secret: "b", APIURL: "http://a", RPCURL: "grpc://a", EnableFunctions: true}
	joined := strings.Join(spec.Env(), "\n")
	if !strings.Contains(joined, "CLIENT_FEATURES_ENABLE_FUNCTIONS=true") {
		t.Fatal("显式打开时应传递 true")
	}
	if !strings.Contains(joined, "CLIENT_FEATURES_ENABLE_REMOTE_SHELL=false") {
		t.Fatal("未显式打开时应显式为 false")
	}
}
