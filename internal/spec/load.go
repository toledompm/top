package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/toledompm/top/internal/domain"
)

// Result is what LoadDir produced: every resolved service (sorted by
// namespace tier, then namespace, then name) plus warnings. Warnings are
// returned even when err is non-nil.
type Result struct {
	Services []domain.Service
	Warnings []Warning
}

// Option tunes LoadDir.
type Option func(*options)

type options struct {
	platformRepo bool
}

// WithPlatformRepo marks the repo being loaded as the platform repo (the one
// TOP's own spec lives in, ADR-0004/0005). Only a platform repo may declare
// specs in reserved namespaces (core, observability); a tenant repo doing so
// is a validation error.
func WithPlatformRepo(v bool) Option { return func(o *options) { o.platformRepo = v } }

// envKeyRe matches a sensible environment variable key.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// cpuQtyRe matches "500m" or "2" style CPU quantities (contract examples).
var cpuQtyRe = regexp.MustCompile(`^([0-9]+)(m)?$`)

// memQtyRe matches "512Mi" / "2Gi" style memory quantities.
var memQtyRe = regexp.MustCompile(`^([0-9]+)(Ki|Mi|Gi|Ti)$`)

// digestRe pins an image digest: sha256:<64 lowercase hex>.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	maxContainerPort = 65535
)

// LoadDir reads every ServiceSpec under dir (repo layout: one YAML file per
// service at <namespace>/<name>.yaml; a root-level file must carry an
// explicit metadata.namespace) and returns resolved domain.Services.
//
// Errors (strict parsing, validation, duplicate identity, reserved-namespace
// misuse) are aggregated with errors.Join, each carrying its file:line.
func LoadDir(dir string, opts ...Option) (Result, error) {
	o := options{}
	for _, fn := range opts {
		fn(&o)
	}

	entries, err := walkSpecFiles(dir)
	if err != nil {
		return Result{}, err
	}

	res := Result{}
	var errs []error
	seen := map[domain.ServiceRef]string{} // identity -> file (collision check)

	for _, rel := range entries {
		full := filepath.Join(dir, rel)
		dirNS := filepath.Base(filepath.Dir(rel))
		if dirNS == filepath.Base(dir) || dirNS == "." {
			dirNS = "" // file at repo root: metadata.namespace is required
		}
		svc, warns, err := loadFile(full, rel, dirNS, o)
		res.Warnings = append(res.Warnings, warns...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if svc.Ref.IsZero() {
			continue // empty/comment-only file
		}
		if prev, dup := seen[svc.Ref]; dup {
			errs = append(errs, parseErr(rel, 0, fmt.Errorf(
				"identity %s already declared in %s: duplicate service (namespace+name must be unique, ADR-0004)",
				svc.Ref, prev)))
			continue
		}
		seen[svc.Ref] = rel
		res.Services = append(res.Services, svc)
	}

	sortServices(res.Services)
	sortWarnings(res.Warnings)
	return res, errors.Join(errs...)
}

// walkSpecFiles returns every relative yaml file path, depth 1 (root) or
// depth 2 (<ns>/<file>), skipping dot-directories and symlinks.
func walkSpecFiles(dir string) ([]string, error) {
	dirEnt, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !dirEnt.IsDir() {
		return nil, fmt.Errorf("spec dir %q: not a directory", dir)
	}
	out := []string{}
	root, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range root {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		switch {
		case e.IsDir():
			sub, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			for _, f := range sub {
				if strings.HasPrefix(f.Name(), ".") || f.Type()&fs.ModeSymlink != 0 ||
					f.IsDir() || !isYAML(f.Name()) {
					continue
				}
				out = append(out, filepath.Join(name, f.Name()))
			}
		case isYAML(name):
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// sortServices orders deterministically: namespace tier (core →
// observability → rest, ADR-0004), then namespace, then service name.
func sortServices(svcs []domain.Service) {
	sort.SliceStable(svcs, func(i, j int) bool {
		a, b := svcs[i], svcs[j]
		if tierA, tierB := a.Ref.Namespace.Tier(), b.Ref.Namespace.Tier(); tierA != tierB {
			return tierA < tierB
		}
		if a.Ref.Namespace != b.Ref.Namespace {
			return a.Ref.Namespace < b.Ref.Namespace
		}
		return a.Ref.Name < b.Ref.Name
	})
}

// sortWarnings orders deterministically by (file, field, code).
func sortWarnings(ws []Warning) {
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Code < b.Code
	})
}

// loadFile decodes one file (full = path for I/O, rel = the repo-relative
// path used in errors/warnings) and validates its document. Returns a nil
// Service for an empty/comment-only file (skip, not error). dirNS is the
// namespace proposed by the file's parent directory ("" for root-level).
func loadFile(full, rel, dirNS string, o options) (domain.Service, []Warning, error) {
	data, err := os.ReadFile(full)
	if err != nil {
		return domain.Service{}, nil, parseErr(rel, 0, err)
	}

	docs, decodeErr := decodeDocs(rel, data)
	if decodeErr != nil {
		return domain.Service{}, nil, decodeErr
	}
	if len(docs) > 1 {
		return domain.Service{}, nil, parseErr(rel, 0,
			fmt.Errorf("multi-document YAML: one spec per file (contract layout: <namespace>/<name>.yaml)"))
	}
	if len(docs) == 0 {
		return domain.Service{}, nil, nil // empty/comment-only file: skip
	}

	svc, warns, err := build(*docs[0], rel, dirNS, o)
	return svc, warns, err
}

// decodeDocs decodes every document strictly (KnownFields) and, in a second
// non-strict pass, captures the yaml.Node of each document for line-number
// reporting. Unknown-field TypeErrors are split into one ParseError per
// message line, each keeping the YAML line number the decoder reports.
func decodeDocs(path string, data []byte) ([]*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var docs []*Document
	for {
		var doc Document
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, wrapDecodeErr(path, err)
		}
		docs = append(docs, &doc)
	}
	// Second pass: raw nodes for line lookups. Can only fail if the first
	// pass failed; a mismatch in document count is impossible.
	nodes := []*yaml.Node{}
	dec2 := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var n yaml.Node
		if err := dec2.Decode(&n); err != nil {
			break
		}
		nodes = append(nodes, &n)
	}
	for i, doc := range docs {
		if i < len(nodes) {
			doc.Raw = nodes[i]
		}
	}
	return docs, nil
}
