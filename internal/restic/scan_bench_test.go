package restic

import (
	"fmt"
	"strings"
	"testing"
)

func benchmarkFindJSON(records int) string {
	var b strings.Builder
	b.WriteString(`[{"hits":`)
	fmt.Fprintf(&b, "%d", records)
	b.WriteString(`,"snapshot":"abcdef0123456789","matches":[`)
	for i := 0; i < records; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"path":"/data/entry-%05d.txt","name":"entry-%05d.txt","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"}`, i, i)
	}
	b.WriteString(`]}]`)
	return b.String()
}

func benchmarkLSJSON(records int) string {
	var b strings.Builder
	for i := 0; i < records; i++ {
		fmt.Fprintf(&b, `{"message_type":"node","name":"entry-%05d.txt","type":"file","path":"/data/entry-%05d.txt","size":1,"permissions":"-rw-------","mtime":"2024-01-01T00:00:00Z"}`+"\n", i, i)
	}
	return b.String()
}

func BenchmarkScanLSBoundedPage(b *testing.B) {
	const records = 10000
	input := benchmarkLSJSON(records)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	var pageRows int
	var rawBytes int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := ScanLS(strings.NewReader(input), ScanLimits{MaxBytes: len(input), MaxDecodedEntries: 100000}, 0, 50, func(Node) bool { return true })
		if err != nil {
			b.Fatal(err)
		}
		pageRows = len(result.Nodes)
		rawBytes = result.Stats.RawBytes
	}
	b.ReportMetric(float64(rawBytes), "rawbytes/op")
	b.ReportMetric(float64(pageRows), "pagerows/op")
}

func BenchmarkScanFindBoundedPage(b *testing.B) {
	const records = 10000
	input := benchmarkFindJSON(records)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	var pageRows int
	var rawBytes int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := ScanFind(strings.NewReader(input), ScanLimits{MaxBytes: len(input), MaxDecodedEntries: 100000}, 0, 50, func(string, FindMatch) bool { return true })
		if err != nil {
			b.Fatal(err)
		}
		pageRows = result.Stats.PageEntries
		rawBytes = result.Stats.RawBytes
	}
	b.ReportMetric(float64(rawBytes), "rawbytes/op")
	b.ReportMetric(float64(pageRows), "pagerows/op")
}
