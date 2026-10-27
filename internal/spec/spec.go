// Package spec parses and validates ServiceSpec YAML files from a platform
// Git repo (docs/contracts/service-spec.md, ADR-0004). It is the only place
// YAML is decoded and defaults are applied; everything downstream (reconciler,
// CLI, store) consumes the resolved domain.Service values it produces.
//
// The loader walks `<dir>/<namespace>/<name>.yaml` (one spec per file, strict
// fields, one document per file) and returns every resolved service plus a
// list of non-fatal warnings (mutable tags, `latest`). All validation errors
// are aggregated with errors.Join; each one carries its `file:line`.
package spec

import (
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// APIVersion and Kind are the only accepted document header values.
const (
	APIVersion  = "top/v1alpha1"
	KindService = "Service"
)

// Document is the YAML-facing shape of one ServiceSpec file. It mirrors the
// contract's field summary verbatim (v0); defaults are applied by Build,
// never by the decoder, so omissions stay observable.
type Document struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`

	// Raw holds the full document node for error line resolution. It is
	// filled in by a second, non-strict decode pass (decodeDocs) — never the
	// strict pass — because an inline yaml.Node clashes with the `kind` key.
	Raw *yaml.Node `yaml:"-"`
}

// Metadata pins immutable identity (ADR-0004): name + namespace map 1:1 to
// the Dokku app "<namespace>-<name>".
type Metadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// Spec is the workload definition: `spec:` in the file.
type Spec struct {
	Source        Source            `yaml:"source"`
	Ports         []Port            `yaml:"ports,omitempty"`
	Routing       Routing           `yaml:"routing,omitempty"`
	HealthCheck   *Health           `yaml:"healthcheck,omitempty"`
	Storage       []Storage         `yaml:"storage,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
	SecretRefs    []string          `yaml:"secretRefs,omitempty"`
	DependsOn     []string          `yaml:"dependsOn,omitempty"`
	Rollout       Rollout           `yaml:"rollout,omitempty"`
	RestartPolicy string            `yaml:"restartPolicy,omitempty"`
	Resources     Resources         `yaml:"resources,omitempty"`
}

// Source is image XOR build; for images, exactly one of tag/digest.
type Source struct {
	Image  string    `yaml:"image,omitempty"`
	Tag    string    `yaml:"tag,omitempty"`
	Digest string    `yaml:"digest,omitempty"`
	Build  *SrcBuild `yaml:"build,omitempty"`
}

// SrcBuild is a Dokku source build (ADR-0002): repo + ref + optional path.
type SrcBuild struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path,omitempty"`
}

// Port is one declared container port. HostPort is discouraged (contract).
type Port struct {
	Name          string `yaml:"name,omitempty"`
	ContainerPort int    `yaml:"containerPort"`
	HostPort      int    `yaml:"hostPort,omitempty"`
}

// Routing is the exposure intent; mode default is "none".
type Routing struct {
	Mode    string   `yaml:"mode,omitempty"`
	Domains []string `yaml:"domains,omitempty"`
}

// Health maps to Dokku checks; a fully empty healthcheck is an error —
// omit the whole key for Dokku defaults. (Contract wording: http/tcp/exec.)
type Health struct {
	HTTP            *HTTPCheck `yaml:"http,omitempty"`
	TCP             *TCPCheck  `yaml:"tcp,omitempty"`
	Exec            *ExecCheck `yaml:"exec,omitempty"`
	IntervalSeconds int        `yaml:"intervalSeconds,omitempty"`
}

// HTTPCheck probes over HTTP; Port is a declared port name or number.
type HTTPCheck struct {
	Path string `yaml:"path"`
	Port string `yaml:"port"`
}

// TCPCheck asserts a port accepts connections.
type TCPCheck struct {
	Port string `yaml:"port"`
}

// ExecCheck runs a command inside the container.
type ExecCheck struct {
	Command []string `yaml:"command"`
}

// Storage is one desired volume mount; size is advisory.
type Storage struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	Size      string `yaml:"size,omitempty"`
}

// Rollout is the rollout policy; MaxUnavailable default 0 means
// zero-downtime checks must pass.
type Rollout struct {
	MaxUnavailable int `yaml:"maxUnavailable,omitempty"`
}

// Resources are string quantities for Dokku's resource plugin.
type Resources struct {
	Limits   Quantity `yaml:"limits,omitempty"`
	Requests Quantity `yaml:"requests,omitempty"`
}

// Quantity is one limits/requests half: cpu + memory strings.
type Quantity struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// ParseError is one spec failure with its source location. Line is 1-based
// for decode/semantic errors found on a node and 0 when unknown.
type ParseError struct {
	Path string // file path
	Line int
	Err  error
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %v", e.Path, e.Line, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// parseErr wraps err with path and, when line>0, forces the line.
func parseErr(path string, line int, err error) error {
	var pe *ParseError
	if errors.As(err, &pe) {
		if pe.Path == "" {
			pe.Path = path
		}
		if line > 0 && pe.Line == 0 {
			pe.Line = line
		}
		return pe
	}
	return &ParseError{Path: path, Line: line, Err: err}
}

// WarningCode classifies a non-fatal validation finding.
type WarningCode string

const (
	// WarnMutableTag: an image tagged without a digest (recommend digest pinning).
	WarnMutableTag WarningCode = "mutable-tag"
	// WarnLatestTag: the image tag is `latest`.
	WarnLatestTag WarningCode = "latest-tag"
	// WarnMutableRef: a build ref does not look like a pinned git sha.
	WarnMutableRef WarningCode = "mutable-ref"
)

// Warning is one non-fatal finding: where (file + field) and what.
type Warning struct {
	Path    string // repo-relative file path
	Field   string // YAML path, e.g. "spec.source.tag"
	Code    WarningCode
	Message string
}
