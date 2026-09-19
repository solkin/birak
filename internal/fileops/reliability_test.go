package fileops

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func wantBody(t *testing.T, path, body string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("%s: got %q (%v), want %q", path, got, err, body)
	}
}
func TestWriterPublicationAndAbort(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("exists=%v/abort=%v", existing, abort), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				if existing {
					mustWrite(t, path, "old bytes")
				}
				f, closeWriter, err := OpenWriter(root, path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteString("new bytes"); err != nil {
					t.Fatal(err)
				}
				if existing {
					wantBody(t, path, "old bytes")
				} else if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("unclosed file published")
				}
				if abort {
					if err = AbortWriter(root, f); err != nil {
						t.Fatal(err)
					}
					if err = closeWriter(); !errors.Is(err, os.ErrClosed) {
						t.Fatalf("close after abort: %v", err)
					}
				} else {
					if err = closeWriter(); err != nil {
						t.Fatal(err)
					}
					if err = closeWriter(); err != nil {
						t.Fatal(err)
					}
				}
				if !abort {
					wantBody(t, path, "new bytes")
				} else if existing {
					wantBody(t, path, "old bytes")
				} else if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("aborted create exists")
				}
				leftovers, _ := filepath.Glob(filepath.Join(root, ".birak-tmp-*"))
				if len(leftovers) > 0 {
					t.Fatalf("leaked staging files: %v", leftovers)
				}
			})
		}
	}
}
func TestWriterAliasesAndNamespace(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "dir", "target")
			alias := filepath.Join(root, "alias")
			mustWrite(t, target, "before")
			var err error
			if kind == "symlink" {
				err = os.Symlink(target, alias)
			} else {
				err = os.Link(target, alias)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, closeWriter, err := OpenWriter(root, alias, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer AbortWriter(root, f)
			if _, _, err = OpenWriter(root, target, os.O_TRUNC|os.O_WRONLY, 0o600); !errors.Is(err, ErrBusy) {
				t.Fatalf("second alias writer: %v", err)
			}
			if err = Remove(root, filepath.Dir(target), true); !errors.Is(err, ErrBusy) {
				t.Fatalf("delete active inode via parent: %v", err)
			}
			if err = Rename(root, target, filepath.Join(root, "moved")); !errors.Is(err, ErrBusy) {
				t.Fatalf("rename active inode: %v", err)
			}
			if _, err = f.WriteAt([]byte("AFTER!"), 0); err != nil {
				t.Fatal(err)
			}
			if err = closeWriter(); err != nil {
				t.Fatal(err)
			}
			wantBody(t, alias, "AFTER!")
			if kind == "symlink" {
				wantBody(t, target, "AFTER!")
			}
		})
	}
}
func TestWriterRefusesExternalReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	mustWrite(t, path, "before")
	f, closeWriter, err := OpenWriter(root, path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer AbortWriter(root, f)
	if _, err = f.WriteAt([]byte("client"), 0); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	mustWrite(t, other, "external")
	if err = os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if err = closeWriter(); !errors.Is(err, ErrBusy) {
		t.Fatalf("close replaced inode: %v", err)
	}
	wantBody(t, path, "external")
}

// Real process termination at the implementation's commit boundaries. No power
// loss or fake filesystem durability model is involved.
func TestReplacementProcessCuts(t *testing.T) {
	if root := os.Getenv("BIRAK_REPLACE_ROOT"); root != "" {
		checkpoint := func(step string) {
			if step == os.Getenv("BIRAK_REPLACE_CUT") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}
		if os.Getenv("BIRAK_REPLACE_RECOVER") == "true" {
			err := recoverLocked(root, checkpoint)
			t.Fatalf("recovery child missed cut: %v", err)
		}
		src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
		move := os.Getenv("BIRAK_REPLACE_MOVE") == "true"
		directory := os.Getenv("BIRAK_REPLACE_DIRECTORY") == "true"
		moveSrc := ""
		if move {
			moveSrc = src
		}
		var build func(string) error
		if !move {
			build = func(stage string) error {
				if directory {
					if err := os.Mkdir(stage, 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(stage, "file"), []byte("new"), 0600)
				}
				return os.WriteFile(stage, []byte("new"), 0600)
			}
		}
		err := replaceLocked(root, moveSrc, dst, build, checkpoint)
		t.Fatalf("child missed cut: %v", err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, move := range []bool{false, true} {
		for _, directory := range []bool{false, true} {
			for _, existing := range []bool{false, true} {
				for _, cut := range []string{"prepared", "backed-up", "published", "committed", "cleaned"} {
					t.Run(fmt.Sprintf("move=%v/dir=%v/exists=%v/%s", move, directory, existing, cut), func(t *testing.T) {
						root := t.TempDir()
						src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
						suffix := ""
						if directory {
							suffix = "/file"
						}
						mustWrite(t, src+suffix, "new")
						if existing {
							mustWrite(t, dst+suffix, "old")
						}
						cmd := exec.Command(binary, "-test.run=^TestReplacementProcessCuts$")
						cmd.Env = append(os.Environ(), "BIRAK_REPLACE_ROOT="+root, fmt.Sprintf("BIRAK_REPLACE_MOVE=%v", move), fmt.Sprintf("BIRAK_REPLACE_DIRECTORY=%v", directory), "BIRAK_REPLACE_CUT="+cut)
						out, err := cmd.CombinedOutput()
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatalf("child: %v %s", err, out)
						}
						status, ok := exit.Sys().(syscall.WaitStatus)
						if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
							t.Fatalf("wrong child failure: %v %s", err, out)
						}
						for range 2 {
							if err = RecoverLocked(root); err != nil {
								t.Fatal(err)
							}
						}
						committed := cut == "committed" || cut == "cleaned"
						if committed {
							wantBody(t, dst+suffix, "new")
						} else if existing {
							wantBody(t, dst+suffix, "old")
						} else if _, err = os.Stat(dst); !os.IsNotExist(err) {
							t.Fatal("uncommitted destination exists")
						}
						if move && committed {
							if _, err = os.Stat(src); !os.IsNotExist(err) {
								t.Fatal("move source survived commit")
							}
						} else {
							wantBody(t, src+suffix, "new")
						}
						entries, _ := os.ReadDir(root)
						for _, e := range entries {
							if strings.HasPrefix(e.Name(), ".birak-bak-") {
								t.Fatalf("backup leaked: %s", e.Name())
							}
						}
					})
				}
			}
		}
	}
}
