package pac

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// benchContent builds a PAC file the size of a real gfwlist-derived one, so the
// benchmarks below measure the per-request and per-count cost the UI actually
// pays rather than a toy payload.
func benchContent(b *testing.B, n int) []byte {
	b.Helper()
	rules := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rules = append(rules, fmt.Sprintf("||bench-domain-%d.example.com", i))
	}
	var body bytes.Buffer
	body.WriteString("var rules = [")
	for i, r := range rules {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, "%q", r)
	}
	body.WriteString("];\n")
	// Pad towards the real file size: abp.js itself is tens of kilobytes.
	body.WriteString(strings.Repeat("// padding line\n", 2000))
	return body.Bytes()
}

// BenchmarkRuleCountFromPAC measures the parse the UI triggers for the PAC tab.
// It runs only when the file changed (see RuleCache), so this is the expensive
// path that has to stay off the hot one.
func BenchmarkRuleCountFromPAC(b *testing.B) {
	content := benchContent(b, 8000)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := RuleCountFromPAC(content); got != 8000 {
			b.Fatalf("规则数错误: %d", got)
		}
	}
}

// BenchmarkPACHandler measures serving one PAC request. Browsers and macOS both
// poll the PAC URL, and the handler re-reads the file on every request by
// design, so the copy here is on a real hot path.
func BenchmarkPACHandler(b *testing.B) {
	content := benchContent(b, 8000)
	s := NewServer(func() ([]byte, error) { return content, nil }, nil)

	req := httptest.NewRequest(http.MethodGet, "/proxy.pac", nil)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		s.handle(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("状态码 %d", rec.Code)
		}
		if rec.Body.Len() != len(content) {
			b.Fatalf("响应长度 %d，期望 %d", rec.Body.Len(), len(content))
		}
	}
}
