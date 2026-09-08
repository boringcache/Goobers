// Package configsource defines the source boundary used by config loaders.
package configsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigSource resolves the current config snapshot to a directory.
type ConfigSource interface {
	Resolve(context.Context) (string, error)
}

// LocalDirSource resolves to a plain directory without modifying or copying it.
type LocalDirSource struct {
	Path string
}

// Resolve returns the configured local directory.
func (s LocalDirSource) Resolve(context.Context) (string, error) {
	return s.Path, nil
}

// SourceKind describes how a configuration repository is addressed by the portal.
type SourceKind string

const (
	SourceKindLocal    SourceKind = "local"
	SourceKindGit      SourceKind = "git"
	SourceKindProvider SourceKind = "provider"
)

// SourceCapabilities advertises the read/write model for a config source.
type SourceCapabilities struct {
	Read        bool `json:"read"`
	Validate    bool `json:"validate"`
	DirectWrite bool `json:"directWrite"`
	ReviewWrite bool `json:"reviewWrite"`
}

// SourceDescriptor is the browser-safe identity and current revision of a
// configuration source.
type SourceDescriptor struct {
	ID           string             `json:"id"`
	DisplayName  string             `json:"displayName"`
	Kind         SourceKind         `json:"kind"`
	Revision     string             `json:"revision"`
	Capabilities SourceCapabilities `json:"capabilities"`
}

// DefinitionKind identifies the authored definition kind behind a logical path.
type DefinitionKind string

const (
	DefinitionManifest DefinitionKind = "manifest"
	DefinitionInstance DefinitionKind = "instance"
	DefinitionGaggle   DefinitionKind = "gaggle"
	DefinitionWorkflow DefinitionKind = "workflow"
	DefinitionGoober   DefinitionKind = "goober"
	DefinitionSupport  DefinitionKind = "support"
)

// DefinitionReference links a logical source document to an authored definition.
type DefinitionReference struct {
	Kind   DefinitionKind `json:"kind"`
	Name   string         `json:"name"`
	Gaggle string         `json:"gaggle,omitempty"`
}

// DocumentDescriptor identifies one logical source document. The path is
// relative to the source root and never a host filesystem path.
type DocumentDescriptor struct {
	Path       string               `json:"path"`
	MediaType  string               `json:"mediaType"`
	ETag       string               `json:"etag"`
	Editable   bool                 `json:"editable"`
	Definition *DefinitionReference `json:"definition,omitempty"`
}

// Document is one source document and the source revision that projected it.
type Document struct {
	SourceID string             `json:"sourceId"`
	Revision string             `json:"revision"`
	Document DocumentDescriptor `json:"document"`
	Content  string             `json:"content"`
}

// DiscoverSource returns the canonical descriptor for a local config source.
func DiscoverSource(ctx context.Context, source ConfigSource) (*SourceDescriptor, error) {
	if source == nil {
		return nil, fmt.Errorf("config source is required")
	}
	root, err := source.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve config source: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve config source root: %w", err)
	}
	revision, err := sourceRevision(root)
	if err != nil {
		return nil, err
	}
	return &SourceDescriptor{
		ID:          "source:local",
		DisplayName: filepath.Base(root),
		Kind:        SourceKindLocal,
		Revision:    revision,
		Capabilities: SourceCapabilities{
			Read:        true,
			Validate:    true,
			DirectWrite: true,
			ReviewWrite: false,
		},
	}, nil
}

// ListDocuments enumerates authorable documents under a config root.
func ListDocuments(ctx context.Context, source ConfigSource) ([]DocumentDescriptor, error) {
	root, err := resolveRoot(source, ctx)
	if err != nil {
		return nil, err
	}
	items := make([]DocumentDescriptor, 0)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if isHiddenPath(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if isHiddenPath(root, path) {
			return nil
		}
		if !isSupportedDocument(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		logicalPath := filepath.ToSlash(filepath.Clean(rel))
		if logicalPath == "." || logicalPath == "" || strings.HasPrefix(logicalPath, "../") || strings.Contains(logicalPath, "\\") {
			return fmt.Errorf("unsupported document path %q", logicalPath)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		description, err := documentDefinition(logicalPath)
		if err != nil {
			return err
		}
		items = append(items, DocumentDescriptor{
			Path:       logicalPath,
			MediaType:  "application/yaml",
			ETag:       hashBytes(data),
			Editable:   true,
			Definition: &description,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, nil
}

// ReadDocument resolves and reads one document by logical source path.
func ReadDocument(ctx context.Context, source ConfigSource, logicalPath string) (*Document, error) {
	if source == nil {
		return nil, fmt.Errorf("config source is required")
	}
	root, err := resolveRoot(source, ctx)
	if err != nil {
		return nil, err
	}
	cleaned, err := normalizeLogicalPath(logicalPath)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, filepath.FromSlash(cleaned))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("resolve document path %q: %w", logicalPath, err)
		}
		return nil, fmt.Errorf("document %q not found", logicalPath)
	}
	if !isWithinRoot(root, resolved) {
		return nil, fmt.Errorf("document path escapes source root: %q", logicalPath)
	}
	if isHiddenPath(root, resolved) {
		return nil, fmt.Errorf("hidden credential material is not allowed in %q", logicalPath)
	}
	if !isSupportedDocument(resolved) {
		return nil, fmt.Errorf("unsupported document type %q", logicalPath)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read document %q: %w", logicalPath, err)
	}
	revision, err := sourceRevision(root)
	if err != nil {
		return nil, err
	}
	descriptor, err := documentDefinition(cleaned)
	if err != nil {
		return nil, err
	}
	return &Document{
		SourceID: "source:local",
		Revision: revision,
		Document: DocumentDescriptor{
			Path:       cleaned,
			MediaType:  "application/yaml",
			ETag:       hashBytes(data),
			Editable:   true,
			Definition: &descriptor,
		},
		Content: string(data),
	}, nil
}

func resolveRoot(source ConfigSource, ctx context.Context) (string, error) {
	if source == nil {
		return "", fmt.Errorf("config source is required")
	}
	root, err := source.Resolve(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve config source: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve config source root: %w", err)
	}
	if root == "" || root == "." {
		return "", fmt.Errorf("config source root is empty")
	}
	return root, nil
}

func normalizeLogicalPath(logicalPath string) (string, error) {
	trimmed := strings.TrimSpace(logicalPath)
	if trimmed == "" {
		return "", fmt.Errorf("document path is required")
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "\\") || strings.Contains(trimmed, "..") {
		return "", fmt.Errorf("path traversal is not allowed: %q", logicalPath)
	}
	cleaned := filepath.Clean(filepath.ToSlash(trimmed))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("document path is invalid: %q", logicalPath)
	}
	return filepath.ToSlash(cleaned), nil
}

func isSupportedDocument(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func isHiddenPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func isWithinRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sourceRevision(root string) (string, error) {
	h := sha256.New()
	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if isHiddenPath(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if isHiddenPath(root, path) || !isSupportedDocument(path) {
			return nil
		}
		files = append(files, path)
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(filepath.ToSlash(rel)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func documentDefinition(logicalPath string) (DefinitionReference, error) {
	cleaned := filepath.ToSlash(filepath.Clean(logicalPath))
	name := strings.TrimSuffix(filepath.Base(cleaned), filepath.Ext(cleaned))
	lower := strings.TrimPrefix(cleaned, "/")
	switch {
	case lower == "manifest.yaml" || lower == "manifest.yml":
		return DefinitionReference{Kind: DefinitionManifest, Name: "manifest"}, nil
	case lower == "instance.yaml" || lower == "instance.yml":
		return DefinitionReference{Kind: DefinitionInstance, Name: "instance"}, nil
	case strings.HasPrefix(lower, "gaggles/"):
		parts := strings.Split(lower, "/")
		if len(parts) >= 4 && parts[0] == "gaggles" {
			return DefinitionReference{Kind: DefinitionGaggle, Name: name, Gaggle: parts[1]}, nil
		}
		return DefinitionReference{Kind: DefinitionGaggle, Name: name}, nil
	case strings.HasPrefix(lower, "workflows/"):
		return DefinitionReference{Kind: DefinitionWorkflow, Name: name}, nil
	case strings.HasPrefix(lower, "goobers/"):
		return DefinitionReference{Kind: DefinitionGoober, Name: name}, nil
	case strings.HasPrefix(lower, "support/"):
		return DefinitionReference{Kind: DefinitionSupport, Name: name}, nil
	default:
		return DefinitionReference{Kind: DefinitionSupport, Name: name}, nil
	}
}
