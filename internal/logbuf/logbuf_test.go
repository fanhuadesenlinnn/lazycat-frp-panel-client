package logbuf

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWriteSplitsLinesAndKeepsPartial(t *testing.T) {
	r := New(10, 1024, nil)
	if _, err := r.Write([]byte("line1\nline2\npart")); err != nil {
		t.Fatal(err)
	}
	lines := r.Since(0)
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d: %+v", len(lines), lines)
	}
	r.Write([]byte("ial\n"))
	lines = r.Since(0)
	if len(lines) != 3 || lines[2].Text != "partial" {
		t.Fatalf("未正确拼接半行: %+v", lines)
	}
}

func TestWriteIgnoresBlankLines(t *testing.T) {
	r := New(10, 1024, nil)
	r.Write([]byte("\n   \n real \n"))
	lines := r.Since(0)
	if len(lines) != 1 || strings.TrimSpace(lines[0].Text) != "real" {
		t.Fatalf("空行应被忽略: %+v", lines)
	}
}

func TestRingIsBounded(t *testing.T) {
	r := New(3, 1024, nil)
	for i := 0; i < 10; i++ {
		r.Write([]byte("line\n"))
	}
	lines := r.Since(0)
	if len(lines) != 3 {
		t.Fatalf("应只保留 3 行，实际 %d", len(lines))
	}
	if lines[0].Seq != 8 || lines[2].Seq != 10 {
		t.Fatalf("保留的应是最新 3 行: %+v", lines)
	}
}

func TestSinceReturnsIncrementalLines(t *testing.T) {
	r := New(10, 1024, nil)
	r.Write([]byte("a\nb\nc\n"))
	all := r.Since(0)
	tail := r.Since(all[0].Seq)
	if len(tail) != 2 || tail[0].Text != "b" {
		t.Fatalf("增量读取不正确: %+v", tail)
	}
	if r.LastSeq() != 3 {
		t.Fatalf("LastSeq = %d", r.LastSeq())
	}
}

func TestRedactionHappensBeforeTruncation(t *testing.T) {
	secret := "SUPERSECRETVALUE"
	red := func(s string) string { return strings.ReplaceAll(s, secret, "***") }
	body := "aaaa" + secret + strings.Repeat("z", 40) + "\n"

	// 截断点正好落在脱敏占位符中间：既要保证密钥不泄漏，也不能把占位符切成半个 "*"。
	narrow := New(10, 6, red)
	narrow.Write([]byte(body))
	got := narrow.Since(0)[0].Text
	if strings.Contains(got, secret) || strings.Contains(got, "SUPERSEC") {
		t.Fatalf("截断发生在脱敏之前，密钥泄漏: %q", got)
	}
	if strings.Contains(got, "*") {
		t.Fatalf("截断不应留下半个占位符: %q", got)
	}
	if got != "aaaa ...(已截断)" {
		t.Fatalf("截断结果不符合预期: %q", got)
	}

	// 更宽的截断点同样必须先脱敏。
	wide := New(10, 20, red)
	wide.Write([]byte(body))
	got = wide.Since(0)[0].Text
	if strings.Contains(got, secret) || strings.Contains(got, "SUPERSEC") {
		t.Fatalf("密钥泄漏: %q", got)
	}
	if !strings.Contains(got, "已截断") {
		t.Fatalf("应标记截断: %q", got)
	}
}

func TestRunawayLineIsDiscarded(t *testing.T) {
	r := New(10, 32, nil)
	r.Write([]byte(strings.Repeat("x", 200)))
	lines := r.Since(0)
	if len(lines) != 1 {
		t.Fatalf("应收到一条提示，实际 %d", len(lines))
	}
	if !strings.Contains(lines[0].Text, "已丢弃") {
		t.Fatalf("提示内容不正确: %q", lines[0].Text)
	}
	// 提示本身不应包含超长内容。按字符数（而非 UTF-8 字节数）判断，因为提示
	// 是中文文本。
	if utf8.RuneCountInString(lines[0].Text) > 64 {
		t.Fatalf("提示本身不应包含超长内容: %d 字符", utf8.RuneCountInString(lines[0].Text))
	}
}

// 超长行被丢弃后，它的“尾巴”（换行符之前的剩余内容）不得作为一行重新出现。
func TestRunawayLineTailIsNotResurrected(t *testing.T) {
	r := New(10, 32, nil)
	r.Write([]byte(strings.Repeat("x", 200)))
	r.Write([]byte("SENSITIVE-TAIL\n"))
	r.Write([]byte("next line\n"))
	r.Flush()

	lines := r.Since(0)
	if len(lines) != 2 {
		t.Fatalf("应只保留提示行与后续正常行，实际 %d: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Text, "已丢弃") {
		t.Fatalf("第一条应是丢弃提示: %q", lines[0].Text)
	}
	if lines[1].Text != "next line" {
		t.Fatalf("第二条应是换行后的新行: %q", lines[1].Text)
	}
	for _, l := range lines {
		if strings.Contains(l.Text, "SENSITIVE-TAIL") {
			t.Fatalf("被丢弃行尾部的数据泄漏到了缓冲: %q", l.Text)
		}
	}
}

// 丢弃状态在 Flush 后不应把残留内容写回缓冲。
func TestFlushDoesNotResurrectDiscardedLine(t *testing.T) {
	r := New(10, 16, nil)
	r.Write([]byte(strings.Repeat("y", 100)))
	r.Flush()
	lines := r.Since(0)
	if len(lines) != 1 || !strings.Contains(lines[0].Text, "已丢弃") {
		t.Fatalf("Flush 后不应写入超长行残留: %+v", lines)
	}
	if strings.Contains(lines[0].Text, "yyy") {
		t.Fatalf("提示中不应包含被丢弃的内容: %q", lines[0].Text)
	}
}

func TestFlushStoresTrailingPartial(t *testing.T) {
	r := New(10, 1024, nil)
	r.Write([]byte("no newline"))
	if len(r.Since(0)) != 0 {
		t.Fatal("未换行的内容不应立即入库")
	}
	r.Flush()
	if len(r.Since(0)) != 1 {
		t.Fatal("Flush 后应写入残留内容")
	}
}

func TestClear(t *testing.T) {
	r := New(10, 1024, nil)
	r.Write([]byte("a\n"))
	r.Clear()
	if len(r.Since(0)) != 0 {
		t.Fatal("清空后不应有内容")
	}
}
