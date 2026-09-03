// llbdump marshals the kernel build graph and prints every op with its
// inputs — the raw material for judging whether the graph is shaped well.
package main

import (
	"context"
	"fmt"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

func main() {
	s := kbuild.DefaultSpec()
	s.SourceSHA256 = "a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c"
	s.HTTPSProxy = "http://127.0.0.1:1"
	s.ProxyCAFile = "ca-bundle.crt"
	st, err := kbuild.KernelLLB(s)
	if err != nil {
		panic(err)
	}
	def, err := st.Marshal(context.Background(), llb.LinuxAmd64)
	if err != nil {
		panic(err)
	}
	short := map[digest.Digest]string{}
	var order []digest.Digest
	ops := map[digest.Digest]*pb.Op{}
	for i, dt := range def.Def {
		var op pb.Op
		if err := op.Unmarshal(dt); err != nil {
			panic(err)
		}
		d := digest.FromBytes(dt)
		short[d] = fmt.Sprintf("v%d", i)
		order = append(order, d)
		ops[d] = &op
	}
	for _, d := range order {
		op := ops[d]
		fmt.Printf("%s (%d bytes)\n", short[d], opSize(op))
		for _, in := range op.Inputs {
			fmt.Printf("   <- %s[%d]\n", short[digest.Digest(in.Digest)], in.Index)
		}
		switch o := op.Op.(type) {
		case *pb.Op_Source:
			fmt.Printf("   SOURCE %s  attrs=%v\n", o.Source.Identifier, keys(o.Source.Attrs))
		case *pb.Op_Exec:
			fmt.Printf("   EXEC argv=%v net=%v\n", o.Exec.Meta.Args, o.Exec.Network)
			for _, m := range o.Exec.Mounts {
				fmt.Printf("     mount %-16s input=%d type=%v ro=%v cacheID=%s selector=%q\n",
					m.Dest, m.Input, m.MountType, m.Readonly, cid(m), m.Selector)
			}
		case *pb.Op_File:
			for _, a := range o.File.Actions {
				if c := a.GetCopy(); c != nil {
					fmt.Printf("   FILE copy %q -> %q (secondary input=%d)\n", c.Src, c.Dest, a.SecondaryInput)
				}
			}
		}
	}
	fmt.Printf("total ops: %d\n", len(order))
}

func opSize(op *pb.Op) int { b, _ := op.Marshal(); return len(b) }
func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
func cid(m *pb.Mount) string {
	if m.CacheOpt != nil {
		return m.CacheOpt.ID
	}
	return ""
}
