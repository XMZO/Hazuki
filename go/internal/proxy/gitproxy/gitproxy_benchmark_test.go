package gitproxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func BenchmarkApplyReplacements(b *testing.B) {
	const upstream = "raw.githubusercontent.com"
	const host = "proxy.local"

	cases := []struct {
		name string
		body string
		dict map[string]string
	}{
		{
			name: "single_rule_100kb",
			body: strings.Repeat(`<a href="https://`+upstream+`/repo">repo</a>`, 2200),
			dict: map[string]string{"$upstream": "$custom_domain"},
		},
		{
			name: "multi_rule_100kb",
			body: strings.Repeat(`<a href="https://`+upstream+`/repo">repo</a><img src="https://cdn.jsdelivr.net/a.png">`, 1400),
			dict: map[string]string{
				"$upstream":           "$custom_domain",
				"cdn.jsdelivr.net":    "assets.proxy.local",
				"https://proxy.local": "https://proxy.local",
			},
		},
		{
			name: "single_rule_2mib",
			body: strings.Repeat(`<a href="https://`+upstream+`/repo">repo</a>`, (2<<20)/48),
			dict: map[string]string{"$upstream": "$custom_domain"},
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = applyReplacements(tc.body, upstream, host, tc.dict)
			}
		})
	}
}

func BenchmarkStreamApplyReplacements(b *testing.B) {
	const upstream = "raw.githubusercontent.com"
	const host = "proxy.local"

	cases := []struct {
		name string
		body []byte
		dict map[string]string
	}{
		{
			name: "single_rule_2mib",
			body: []byte(strings.Repeat(`<a href="https://`+upstream+`/repo">repo</a>`, (2<<20)/48)),
			dict: map[string]string{"$upstream": "$custom_domain"},
		},
		{
			name: "multi_rule_2mib",
			body: []byte(strings.Repeat(`<a href="https://`+upstream+`/repo">repo</a><img src="https://cdn.jsdelivr.net/a.png">`, (2<<20)/88)),
			dict: map[string]string{
				"$upstream":        "$custom_domain",
				"cdn.jsdelivr.net": "assets.proxy.local",
			},
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				reader := bytes.NewReader(tc.body)
				if err := streamApplyReplacementsWithChunkSize(io.Discard, reader, upstream, host, tc.dict, maxStreamRewriteChunkBytes); err != nil {
					b.Fatalf("streamApplyReplacementsWithChunkSize: %v", err)
				}
			}
		})
	}
}
