// Package kbuild builds Linux kernels through BuildKit. A Spec describes the
// build; KernelLLB turns it into an LLB graph; GatewaySolve runs that graph
// as a gateway frontend (the #syntax= path) or through the client driver
// (Build). ParseKernelfile reads the Kernelfile format that docker build
// delegates to the frontend image.
package kbuild
