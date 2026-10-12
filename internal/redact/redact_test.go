package redact

import (
	"strings"
	"testing"
)

func TestLiteralRedactionIncludesShortSecrets(t *testing.T) {
	r := New("abc")
	got := r.Redact("client secret is abc, again abc")
	if strings.Contains(got, "abc") {
		t.Fatalf("短密钥也必须脱敏，实际: %q", got)
	}
	if got != "client secret is ***, again ***" {
		t.Fatalf("脱敏结果不符合预期: %q", got)
	}
}

func TestLiteralRedactionPrefersLongestMatch(t *testing.T) {
	r := New("secret", "secret-extended-1234")
	got := r.Redact("value=secret-extended-1234")
	if strings.Contains(got, "extended-1234") || strings.Contains(got, "secret") {
		t.Fatalf("应优先匹配更长的密钥: %q", got)
	}
}

func TestKeyValuePatterns(t *testing.T) {
	r := New()
	cases := map[string]string{
		`joining with secret=abcdef123456`:       "abcdef123456",
		`{"client_secret": "abcdef123456"}`:      "abcdef123456",
		`token: abcdef123456`:                    "abcdef123456",
		`password = abcdef123456`:                "abcdef123456",
		`Authorization: Bearer abcdef1234567890`: "abcdef1234567890",
		`--secret=abcdef123456`:                  "abcdef123456",
		`--secret abcdef123456`:                  "abcdef123456",
		`-s abcdef123456`:                        "abcdef123456",
	}
	for input, secret := range cases {
		got := r.Redact(input)
		if strings.Contains(got, secret) {
			t.Errorf("Redact(%q) 未脱敏: %q", input, got)
		}
	}
}

func TestNoFalsePositiveOnPlainSentence(t *testing.T) {
	r := New()
	for _, in := range []string{
		"client secret cannot be empty",
		"waiting for token refresh",
		"password authentication failed",
	} {
		if got := r.Redact(in); got != in {
			t.Errorf("普通句子不应被改动：%q -> %q", in, got)
		}
	}
}

func TestEmptyAndDuplicateLiterals(t *testing.T) {
	r := New("", "  ", "dup-value")
	r.AddLiterals("dup-value")
	got := r.Redact("x dup-value y")
	if strings.Contains(got, "dup-value") {
		t.Fatalf("未脱敏: %q", got)
	}
}

func TestNilRedactorIsSafe(t *testing.T) {
	var r *Redactor
	if got := r.Redact("abc"); got != "abc" {
		t.Fatalf("nil Redactor 应原样返回，实际 %q", got)
	}
}
