package configsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDirSourceResolve(t *testing.T) {
	source := LocalDirSource{Path: "does/not/need/to/exist"}

	got, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != source.Path {
		t.Fatalf("Resolve() = %q, want %q", got, source.Path)
	}
}

func TestListDocumentsAndReadDocument(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "manifest.yaml"), "apiVersion: goobers.dev/v1alpha1\nkind: Manifest\n")
	mustWrite(t, filepath.Join(root, "gaggles", "core", "gaggle.yaml"), "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\n")
	mustWrite(t, filepath.Join(root, "workflows", "deploy.yaml"), "apiVersion: goobers.dev/v1alpha1\nkind: Workflow\n")
	mustWrite(t, filepath.Join(root, ".env"), "SECRET=1\n")
	mustWrite(t, filepath.Join(root, "notes.txt"), "ignore me\n")

	descriptor, err := DiscoverSource(context.Background(), LocalDirSource{Path: root})
	if err != nil {
		t.Fatalf("DiscoverSource: %v", err)
	}
	if descriptor.Kind != SourceKindLocal || !descriptor.Capabilities.Read {
		t.Fatalf("DiscoverSource = %+v, want a readable local source", descriptor)
	}

	items, err := ListDocuments(context.Background(), LocalDirSource{Path: root})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("ListDocuments() = %d documents, want 3", len(items))
	}

	doc, err := ReadDocument(context.Background(), LocalDirSource{Path: root}, "gaggles/core/gaggle.yaml")
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	if !strings.Contains(doc.Content, "kind: Gaggle") {
		t.Fatalf("ReadDocument content = %q, want Gaggle content", doc.Content)
	}
	if doc.Document.Definition == nil || doc.Document.Definition.Kind != DefinitionGaggle {
		t.Fatalf("ReadDocument definition = %+v, want Gaggle kind", doc.Document.Definition)
	}
}

func TestReadDocumentRejectsTraversalSymlinksAndHiddenFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "gaggles", "core", "safe.yaml"), "kind: Gaggle\n")
	outside := filepath.Join(t.TempDir(), "escape.yaml")
	mustWrite(t, outside, "kind: Gaggle\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "gaggles", "core")), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "gaggles", "core", "escape.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ReadDocument(context.Background(), LocalDirSource{Path: root}, "../etc/passwd"); err == nil {
		t.Fatal("ReadDocument accepted traversal path")
	}
	if _, err := ReadDocument(context.Background(), LocalDirSource{Path: root}, "gaggles/core/escape.yaml"); err == nil {
		t.Fatal("ReadDocument accepted symlink escape")
	}
	if err := os.MkdirAll(filepath.Join(root, ".secrets"), 0o755); err != nil {
		t.Fatalf("mkdir .secrets: %v", err)
	}
	mustWrite(t, filepath.Join(root, ".secrets", "token.yaml"), "token: secret\n")
	if _, err := ReadDocument(context.Background(), LocalDirSource{Path: root}, ".secrets/token.yaml"); err == nil {
		t.Fatal("ReadDocument accepted hidden credential material")
	}
}

func TestSourceRevisionChangesWhenContentsChange(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "manifest.yaml"), "kind: Manifest\n")
	a, err := DiscoverSource(context.Background(), LocalDirSource{Path: root})
	if err != nil {
		t.Fatalf("DiscoverSource first pass: %v", err)
	}
	mustWrite(t, filepath.Join(root, "manifest.yaml"), "kind: Manifest\nversion: v2\n")
	b, err := DiscoverSource(context.Background(), LocalDirSource{Path: root})
	if err != nil {
		t.Fatalf("DiscoverSource second pass: %v", err)
	}
	if a.Revision == b.Revision {
		t.Fatal("source revision should change when the source content changes")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
