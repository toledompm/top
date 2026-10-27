package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/toledompm/top/internal/domain"
)

// typeErrLineRe extracts "line N: msg" entries from yaml decoder TypeErrors.
var typeErrLineRe = regexp.MustCompile(`(?:^|\n|\s)line (\d+): (.*)`)

// wrapDecodeErr turns a strict-decode failure into ParseErrors. yaml TypeError
// messages carry one "line N: …" entry per bad field; other decode errors
// mention their line inline.
func wrapDecodeErr(path string, err error) error {
	var terr *yaml.TypeError
	if !errors.As(err, &terr) {
		return parseErr(path, lineFromMsg(err.Error()), err)
	}
	var errs []error
	for _, msg := range terr.Errors {
		m := typeErrLineRe.FindStringSubmatch(msg)
		if m == nil {
			errs = append(errs, parseErr(path, 0, errors.New(strings.TrimPrefix(msg, "yaml: "))))
			continue
		}
		line, _ := strconv.Atoi(m[1])
		errs = append(errs, parseErr(path, line, errors.New(strings.TrimSpace(m[2]))))
	}
	return errors.Join(errs...)
}

// lineFromMsg extracts "yaml: line N: ..." style lines from plain decode errors.
func lineFromMsg(msg string) int {
	if m := regexp.MustCompile(`line (\d+):`).FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// nodeLine walks the document node along path (mapping keys) and returns the
// line where the final key is declared, or 0 when absent.
func nodeLine(n *yaml.Node, path ...string) int {
	cur := n
	for _, key := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return 0
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == key {
				next = cur.Content[i+1]
				break
			}
		}
		if next == nil {
			return 0
		}
		cur = next
	}
	return cur.Line
}

// build resolves one decoded Document into a domain.Service: header checks,
// identity, namespace rules (dir vs metadata, reserved-namespace protection),
// field validation, and defaults (routing none, restartPolicy always).
func build(doc Document, path, dirNS string, o options) (domain.Service, []Warning, error) {
	if err := header(doc); err != nil {
		return domain.Service{}, nil, parseErr(path, 0, err)
	}

	ns := dirNS
	if doc.Metadata.Namespace != "" {
		if dirNS != "" && doc.Metadata.Namespace != dirNS {
			return domain.Service{}, nil, parseErr(path, nodeLine(doc.Raw, "metadata", "namespace"), fmt.Errorf(
				"metadata.namespace %q does not match file layout namespace %q (file lives at <namespace>/<name>.yaml)",
				doc.Metadata.Namespace, dirNS))
		}
		ns = doc.Metadata.Namespace
	}
	if ns == "" {
		return domain.Service{}, nil, parseErr(path, nodeLine(doc.Raw, "metadata"), errors.New(
			"root-level file requires metadata.namespace (layout is <namespace>/<name>.yaml)"))
	}
	if !domain.Namespace(ns).IsValid() {
		return domain.Service{}, nil, parseErr(path, nodeLine(doc.Raw, "metadata", "namespace"), fmt.Errorf(
			"namespace %q: %w", ns, domain.ErrInvalid))
	}
	if domain.Namespace(ns).IsReserved() && !o.platformRepo {
		return domain.Service{}, nil, parseErr(path, nodeLine(doc.Raw, "metadata", "namespace"), fmt.Errorf(
			"namespace %q is reserved for the platform repo (ADR-0004); a tenant repo may not claim it", ns))
	}

	svc := domain.Service{Ref: domain.Ref(domain.Namespace(ns), domain.Name(doc.Metadata.Name))}
	if err := svc.Ref.Validate(); err != nil { // name syntax + AppName ≤48-char budget
		return domain.Service{}, nil, parseErr(path, nodeLine(doc.Raw, "metadata", "name"), err)
	}

	var warns []Warning
	var errs []error
	add := func(yamlPath []string, err error) {
		if err != nil {
			errs = append(errs, parseErr(path, nodeLine(doc.Raw, yamlPath...), err))
		}
	}

	add([]string{"spec", "source"}, buildSource(doc.Spec.Source, path, &svc, &warns))
	add([]string{"spec", "ports"}, buildPorts(doc.Spec, &svc))
	add([]string{"spec", "routing"}, buildRouting(doc.Spec.Routing, &svc))
	hc, err := buildHealth(doc.Spec.HealthCheck, &svc)
	if err != nil {
		add([]string{"spec", "healthcheck"}, err)
	} else {
		svc.HealthCheck = hc
	}
	add([]string{"spec", "storage"}, buildStorage(doc.Spec.Storage, &svc))
	add([]string{"spec", "env"}, buildEnvVars(doc.Spec, &svc))
	add([]string{"spec", "dependsOn"}, buildDependsOn(doc.Spec.DependsOn, domain.Namespace(ns), &svc))

	if doc.Spec.Rollout.MaxUnavailable < 0 {
		add([]string{"spec", "rollout"}, errors.New(
			"rollout.maxUnavailable: must be >= 0 (0 = require zero-downtime checks to pass)"))
	}
	svc.Rollout = domain.Rollout{MaxUnavailable: doc.Spec.Rollout.MaxUnavailable}

	if doc.Spec.RestartPolicy != "" {
		if p, err := domain.ParseRestartPolicy(doc.Spec.RestartPolicy); err != nil {
			add([]string{"spec", "restartPolicy"}, err)
		} else {
			svc.RestartPolicy = p
		}
	} else {
		svc.RestartPolicy = domain.RestartAlways // spec default (contract table)
	}

	add([]string{"spec", "resources"}, buildResources(doc.Spec.Resources, &svc))
	svc.Annotations = doc.Metadata.Annotations
	return svc, warns, errors.Join(errs...)
}

// header enforces the versioned document header and required name.
func header(doc Document) error {
	if doc.APIVersion != APIVersion || doc.Kind != KindService {
		return fmt.Errorf("apiVersion/kind must be %s/%s, got %s/%s",
			APIVersion, KindService, strOrDefault(doc.APIVersion, "<missing>"), strOrDefault(doc.Kind, "<missing>"))
	}
	if doc.Metadata.Name == "" {
		return errors.New("metadata.name is required")
	}
	return nil
}

// buildSource validates image-xor-build, digest-xor-tag, and emits mutable-tag
// warnings (no digest, or tag `latest`, or non-sha build ref).
func buildSource(src Source, path string, svc *domain.Service, warns *[]Warning) error {
	warn := func(field, code, msg string) {
		*warns = append(*warns, Warning{Path: path, Field: field, Code: WarningCode(code), Message: msg})
	}

	if (src.Image == "") == (src.Build == nil) {
		return fmt.Errorf("%w: source: set exactly one of image or build", domain.ErrInvalid)
	}

	if src.Build != nil {
		b := src.Build
		switch {
		case b.Repo == "":
			return errors.New("source.build.repo: is required for a source build")
		case b.Ref == "":
			return errors.New("source.build.ref: is required for a source build (commit sha recommended)")
		case src.Tag != "" || src.Digest != "":
			return errors.New("source.tag/digest: only valid with an image source, not a build")
		}
		if domain.ValidRevision(b.Ref) != nil {
			warn("spec.source.build.ref", string(WarnMutableRef),
				fmt.Sprintf("build.ref %q does not look like a pinned git sha; mutable refs are allowed but drift-prone", b.Ref))
		}
		svc.Source = domain.Source{Build: &domain.BuildSource{Repo: b.Repo, Ref: b.Ref, Path: b.Path}}
		return nil
	}

	if src.Image == "" {
		return errors.New("source.image: is required when no build source is set")
	}
	switch {
	case src.Tag != "" && src.Digest != "":
		return errors.New("source: set exactly one of tag or digest (digest recommended)")
	case src.Tag == "latest":
		warn("spec.source.tag", string(WarnLatestTag),
			"tag latest is mutable; pin a digest for reproducible reconciliations")
	case src.Digest == "":
		if src.Tag != "" {
			warn("spec.source.tag", string(WarnMutableTag),
				fmt.Sprintf("tag %q is not digest-pinned; pinning a digest is recommended", src.Tag))
		} else {
			warn("spec.source", string(WarnMutableTag),
				"image without tag or digest deploys the implicit latest; pin a digest or tag")
		}
	case !digestRe.MatchString(src.Digest):
		return errors.New("source.digest: want \"sha256:<64 hex chars>\"")
	}
	svc.Source = domain.Source{Image: src.Image, Tag: src.Tag, Digest: src.Digest}
	return nil
}

// buildPorts validates container ports (range, names, duplicates).
func buildPorts(s Spec, svc *domain.Service) error {
	names := map[string]bool{}
	for _, p := range s.Ports {
		if p.ContainerPort < 1 || p.ContainerPort > maxContainerPort {
			return fmt.Errorf("%w: containerPort %d out of range 1..%d", domain.ErrInvalid, p.ContainerPort, maxContainerPort)
		}
		if p.Name != "" {
			if domain.ValidName(p.Name) != nil {
				return fmt.Errorf("%w: port name %q", domain.ErrInvalid, p.Name)
			}
			if names[p.Name] {
				return fmt.Errorf("duplicate port name %q", p.Name)
			}
			names[p.Name] = true
		}
		svc.Ports = append(svc.Ports, domain.Port{Name: p.Name, ContainerPort: p.ContainerPort, HostPort: p.HostPort})
	}
	return nil
}

// buildRouting validates the mode enum; proxy requires non-empty domains.
// Default mode is none. Unknown modes fail (no fallback).
func buildRouting(r Routing, svc *domain.Service) error {
	mode := domain.RoutingNone
	if r.Mode != "" {
		m, err := domain.ParseRoutingMode(r.Mode)
		if err != nil {
			return err
		}
		mode = m
	}
	if mode == domain.RoutingProxy && len(r.Domains) == 0 {
		return errors.New("routing.mode proxy: requires routing.domains (non-empty)")
	}
	for _, d := range r.Domains {
		if strings.TrimSpace(d) == "" {
			return fmt.Errorf("%w: routing.domains: empty domain", domain.ErrInvalid)
		}
	}
	svc.Routing = domain.Routing{Mode: mode, Domains: append([]string(nil), r.Domains...)}
	return nil
}

// buildHealth maps the healthcheck; a present-but-empty healthcheck is an
// error (omit the key to ask for Dokku defaults). healthcheck port references
// are checked against declared ports (by name or containerPort number).
func buildHealth(h *Health, svc *domain.Service) (*domain.HealthCheck, error) {
	if h == nil {
		return nil, nil
	}
	if h.HTTP == nil && h.TCP == nil && h.Exec == nil {
		return nil, errors.New("healthcheck: empty; omit the key to use Dokku defaults")
	}
	if h.IntervalSeconds < 0 {
		return nil, errors.New("healthcheck: intervalSeconds must be >= 0")
	}
	d := domain.HealthCheck{IntervalSeconds: h.IntervalSeconds}
	matchesPort := func(port string) error {
		if port == "" {
			return errors.New("healthcheck port: is required")
		}
		for _, p := range svc.Ports {
			if p.Name == port || strconv.Itoa(p.ContainerPort) == port {
				return nil
			}
		}
		return fmt.Errorf("healthcheck port %q does not match any declared spec.ports entry (name or containerPort)", port)
	}
	if h.HTTP != nil {
		if h.HTTP.Path == "" {
			return nil, errors.New("healthcheck.http.path: is required")
		}
		if !strings.HasPrefix(h.HTTP.Path, "/") {
			return nil, fmt.Errorf("healthcheck.http.path %q: must start with '/'", h.HTTP.Path)
		}
		if err := matchesPort(h.HTTP.Port); err != nil {
			return nil, fmt.Errorf("healthcheck.http: %w", err)
		}
		d.HTTP = &domain.HTTPCheck{Path: h.HTTP.Path, Port: h.HTTP.Port}
	}
	if h.TCP != nil {
		if err := matchesPort(h.TCP.Port); err != nil {
			return nil, fmt.Errorf("healthcheck.tcp: %w", err)
		}
		d.TCP = &domain.TCPCheck{Port: h.TCP.Port}
	}
	if h.Exec != nil {
		if len(h.Exec.Command) == 0 {
			return nil, errors.New("healthcheck.exec.command: is required and may not be empty")
		}
		d.Exec = &domain.ExecCheck{Command: append([]string(nil), h.Exec.Command...)}
	}
	return &d, nil
}

// storageSizeRe matches contract-style advisory sizes ("2Gi", "10G", "512Mi").
var storageSizeRe = regexp.MustCompile(`^[0-9]+(?:[KMGT](?:i)?)?$`)

// buildStorage validates names and absolute mount paths.
func buildStorage(list []Storage, svc *domain.Service) error {
	names := map[string]bool{}
	for _, st := range list {
		switch {
		case st.Name == "":
			return errors.New("storage.name: is required")
		case !strings.HasPrefix(st.MountPath, "/"):
			return fmt.Errorf("storage %q: mountPath %q must be an absolute path (start with '/')", st.Name, st.MountPath)
		case st.Size != "" && !storageSizeRe.MatchString(st.Size):
			return fmt.Errorf("storage %q: size %q: want a plain quantity like 2Gi or 10G", st.Name, st.Size)
		case names[st.Name]:
			return fmt.Errorf("storage: duplicate name %q", st.Name)
		}
		names[st.Name] = true
		svc.Storage = append(svc.Storage, domain.Storage{Name: st.Name, MountPath: st.MountPath, Size: st.Size})
	}
	return nil
}

// buildEnvVars validates plain env keys and secretRef names (ADR-0008: no
// secret values in Git — env is for plain values only).
func buildEnvVars(s Spec, svc *domain.Service) error {
	if svc.Env == nil {
		svc.Env = map[string]string{} // explicit default: {} (contract)
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%w: env key %q: want [A-Za-z_][A-Za-z0-9_]*", domain.ErrInvalid, k)
		}
		// Values are plain strings only; sensitive values belong in the secret
		// store addressed by spec.secretRefs.
		svc.Env[k] = v
	}
	for _, ref := range s.SecretRefs {
		if err := domain.ValidName(ref); err != nil {
			return fmt.Errorf("secretRefs entry %q: %w", ref, err)
		}
		svc.SecretRefs = append(svc.SecretRefs, ref)
	}
	return nil
}

// buildDependsOn parses "ns/name" (or bare "name" = same namespace) refs.
// Cross-namespace dependsOn is rejected: ADR-0004 keeps dependsOn an
// intra-namespace ordering hint; namespace tier ordering is separate.
func buildDependsOn(list []string, ns domain.Namespace, svc *domain.Service) error {
	for _, raw := range list {
		var ref domain.ServiceRef
		if strings.Contains(raw, "/") {
			var err error
			if ref, err = domain.ParseServiceRef(raw); err != nil {
				return fmt.Errorf("dependsOn %q: %w", raw, err)
			}
			if ref.Namespace != ns {
				return fmt.Errorf("dependsOn %q: cross-namespace dependencies are not supported (ADR-0004): wants %s from %s", raw, ref.String(), ns)
			}
		} else {
			if err := domain.ValidName(raw); err != nil {
				return fmt.Errorf("dependsOn %q: %w", raw, err)
			}
			ref = domain.Ref(ns, domain.Name(raw))
		}
		svc.DependsOn = append(svc.DependsOn, ref)
	}
	return nil
}

// buildResources validates and copies cpu/memory quantities per half.
func buildResources(r Resources, svc *domain.Service) error {
	check := func(half string, q Quantity) error {
		if q.CPU != "" && !cpuQtyRe.MatchString(q.CPU) {
			return fmt.Errorf("resources.%s.cpu %q: want plain integer cores or millicores (\"2\", \"500m\")", half, q.CPU)
		}
		if q.Memory != "" && !memQtyRe.MatchString(q.Memory) {
			return fmt.Errorf("resources.%s.memory %q: want byte quantities with binary suffix (\"512Mi\", \"2Gi\")", half, q.Memory)
		}
		return nil
	}
	if err := check("limits", r.Limits); err != nil {
		return err
	}
	if err := check("requests", r.Requests); err != nil {
		return err
	}
	svc.Resources = domain.Resources{Limits: domain.ResourceLimits{CPU: r.Limits.CPU, Memory: r.Limits.Memory}}
	return nil
}

func strOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
