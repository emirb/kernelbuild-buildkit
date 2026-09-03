package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTargets(t *testing.T) {
	t.Setenv("KARCH", "")
	t.Setenv("TARGETS", "vmlinux")
	mk, arts, mods, cfg, _, err := resolveTargets()
	if err != nil || len(mk) != 1 || mk[0] != "vmlinux" || arts["vmlinux"] != "/out/vmlinux" || mods || cfg {
		t.Errorf("default: %v %v %v %v %v", mk, arts, mods, cfg, err)
	}

	t.Setenv("TARGETS", "vmlinux,image,modules,config")
	mk, arts, mods, cfg, _, err = resolveTargets()
	if err != nil || arts["arch/x86/boot/bzImage"] != "/out/bzImage" || !mods || !cfg {
		t.Errorf("x86 full set: %v %v %v %v %v", mk, arts, mods, cfg, err)
	}
	var hasModules bool
	for _, m := range mk {
		if m == "modules" {
			hasModules = true
		}
	}
	if !hasModules {
		t.Errorf("modules target missing from make targets: %v", mk)
	}

	t.Setenv("KARCH", "arm64")
	t.Setenv("TARGETS", "vmlinux,image")
	_, arts, _, _, _, _ = resolveTargets()
	if arts["arch/arm64/boot/Image"] != "/out/Image" || arts["vmlinux"] != "/out/vmlinux" {
		t.Errorf("arm64: %v", arts)
	}

	t.Setenv("KARCH", "")
	t.Setenv("TARGETS", "config")
	mk, _, _, cfg, _, err = resolveTargets()
	if err != nil || len(mk) != 0 || !cfg {
		t.Errorf("config-only must compile nothing: mk=%v cfg=%v err=%v", mk, cfg, err)
	}

	// Outside the frontend contract: empty and unknown must ERROR, never
	// "successfully build nothing".
	t.Setenv("TARGETS", "")
	if _, _, _, _, _, err := resolveTargets(); err == nil {
		t.Error("empty TARGETS accepted")
	}
	t.Setenv("TARGETS", "vmlinux,bzImage")
	if _, _, _, _, _, err := resolveTargets(); err == nil {
		t.Error("unknown TARGETS token accepted")
	}
}

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256(p)
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err != nil || got != want {
		t.Errorf("fileSHA256 = %q, %v", got, err)
	}
	if _, err := fileSHA256(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestStampWantWithoutPatches(t *testing.T) {
	// stampWant's env contract in URL mode without patches.
	t.Setenv("APPLY_PATCHES", "0")
	t.Setenv("SRC_ID", "sha-abc")
	want, err := stampWant()
	if err != nil || want != "src=sha-abc patches=none" {
		t.Errorf("stampWant = %q, %v", want, err)
	}
}
