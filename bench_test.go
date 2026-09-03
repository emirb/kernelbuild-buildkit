package kbuild

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
)

// BenchmarkKernelLLBMarshal: the pure client-side cost of a solve request —
// validate the Spec, generate the graph, marshal to protobuf. Everything
// before the first byte reaches buildkitd.
func BenchmarkKernelLLBMarshal(b *testing.B) {
	s := DefaultSpec()
	s.SourceSHA256 = strings.Repeat("ab", 32)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st, err := KernelLLB(s)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := st.Marshal(ctx, llb.LinuxAmd64); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseKernelfile(b *testing.B) {
	src := "KERNEL 6.18.20\nCONFIG kernel.config\nSHA256 " + strings.Repeat("ab", 32) + "\nTARGETS vmlinux image config\n"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := DefaultSpec()
		if err := ParseKernelfile(strings.NewReader(src), &s); err != nil {
			b.Fatal(err)
		}
	}
}
