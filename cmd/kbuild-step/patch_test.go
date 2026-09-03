package main

import (
	"os"
	"path/filepath"
	"testing"
)

const newExecPatch = `diff --git a/scripts/newtool.sh b/scripts/newtool.sh
new file mode 100755
--- /dev/null
+++ b/scripts/newtool.sh
@@ -0,0 +1,2 @@
+#!/bin/sh
+echo hi
`

const modeOnlyPatch = `diff --git a/tool.sh b/tool.sh
old mode 100644
new mode 100755
`

// TestApplyPatchesHonorsModes: git patches carry file modes. A new file with
// "new file mode 100755" must land executable, and a mode-only patch (no
// hunks) must change the mode rather than being a silent no-op that still
// updates the stamp as if the patch had applied.
func TestApplyPatchesHonorsModes(t *testing.T) {
	t.Run("new-file-mode", func(t *testing.T) {
		build, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(patches, "0001.patch"), []byte(newExecPatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(filepath.Join(build, "scripts", "newtool.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("new file mode 100755 landed as %v (not executable)", st.Mode().Perm())
		}
	})
	t.Run("mode-only", func(t *testing.T) {
		build, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(build, "tool.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(patches, "0001.patch"), []byte(modeOnlyPatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(filepath.Join(build, "tool.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("mode-only patch left %v, want 0755", st.Mode().Perm())
		}
	})
}
