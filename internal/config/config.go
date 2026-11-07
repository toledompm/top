// Package config loads and validates TOP's own runtime configuration
// (top-config.yaml on the controller host).
//
// Load order per ADR conventions and first principles:
//
//	CLI flags (applied by callers via Overlay) > environment (TOP_*) >
//	config file > package defaults.
//
// The file is versioned (`apiVersion: top/v1alpha1`) and strictly parsed:
// unknown fields and a mismatched apiVersion are hard errors (typos must
// fail loudly, mirroring the ServiceSpec contract). All values are JSON
// marshal-friendly for `topctl ... -o json`.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// APIVersion is the only config apiVersion this build accepts.
const APIVersion = "top/v1alpha1"

// ReservedNamespaces are TOP-owned logical namespaces (ADR-0004). They may
// not be removed from the reserved list by configuration.
var ReservedNamespaces = []string{"core", "observability"}

// NamespacePolicyOrder is the fixed reconcile ordering of namespaces.
var NamespacePolicyOrder = []string{"core", "observability", "default"}

// SecretsProviderKind enumerates known secret provider kinds. "vault" is
// known but intentionally unimplemented in v0 (ADR-0008).
type SecretsProviderKind string

const (
	SecretsProviderHostfile SecretsProviderKind = "hostfile"
	SecretsProviderVault    SecretsProviderKind = "vault"
)

// Config is TOP's own versioned runtime configuration.
type Config struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`

	Controller ControllerConfig `yaml:"controller" json:"controller"`
	Git        GitConfig        `yaml:"git" json:"git"`

	Namespaces      NamespacesConfig      `yaml:"namespaces" json:"namespaces"`
	SecretsProvider SecretsProviderConfig `yaml:"secretsProvider" json:"secretsProvider"`

	DataDir  string `yaml:"dataDir" json:"dataDir"`
	LogLevel string `yaml:"logLevel" json:"logLevel"`
}

// ControllerConfig holds the controller loop intervals (ADR-0002/0003).
type ControllerConfig struct {
	PollIntervalSeconds      int `yaml:"pollIntervalSeconds" json:"pollIntervalSeconds"`
	ReconcileIntervalSeconds int `yaml:"reconcileIntervalSeconds" json:"reconcileIntervalSeconds"`
}

// GitConfig describes the platform Git repository (ADR-0002, ADR-0008).
type GitConfig struct {
	URL    string `yaml:"url" json:"url"`
	Branch string `yaml:"branch" json:"branch"`
}

// NamespacesConfig names the TOP-owned namespaces and their ordering.
type NamespacesConfig struct {
	// Reserved is the list of namespaces only the platform repo may use.
	// "core" and "observability" are immutable members.
	Reserved []string `yaml:"reserved" json:"reserved"`
	// Order is the fixed reconcile order across namespaces.
	Order []string `yaml:"order" json:"order"`
}

// SecretsProviderConfig selects the secret provider implementation.
type SecretsProviderConfig struct {
	Kind SecretsProviderKind `yaml:"kind" json:"kind"`
}

// FieldError is one typed validation failure, with the config field path.
type FieldError struct {
	Field string
	Err   error
}

func (e FieldError) Error() string { return fmt.Sprintf("config: %s: %v", e.Field, e.Err) }

// ValidationError carries all field-level validation failures at once.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return fmt.Sprintf("config: invalid: %s", strings.Join(parts, "; "))
}

// Sentinel errors for programmatic classification.
var (
	// ErrAPIVersion is returned when the file's apiVersion mismatches.
	ErrAPIVersion = errors.New("config: unsupported apiVersion")
	// ErrUnknownField is returned when strict parsing hits an unknown field.
	ErrUnknownField = errors.New("config: unknown field")
)

// Default returns the fully-defaulted configuration (ADR-0002/0003 for the
// intervals, ADR-0008 for secrets, ADR-0004/0010 for
// data paths). Tests and tier-3 CI override DataDir.
func Default() Config {
	return Config{
		APIVersion: APIVersion,
		Controller: ControllerConfig{
			PollIntervalSeconds:      60,
			ReconcileIntervalSeconds: 3600,
		},
		Git: GitConfig{
			URL:    "",
			Branch: "main",
		},
		Namespaces: NamespacesConfig{
			Reserved: append([]string(nil), ReservedNamespaces...),
			Order:    append([]string(nil), NamespacePolicyOrder...),
		},
		SecretsProvider: SecretsProviderConfig{
			Kind: SecretsProviderHostfile,
		},
		DataDir:  "/var/lib/top",
		LogLevel: "info",
	}
}

// Load reads path (may be empty → pure defaults + env), applies environment
// overrides (TOP_*), and validates. It never reads or writes secrets.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied
		if err != nil {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true) // strict: unknown fields fail loudly
		if err := dec.Decode(&cfg); err != nil {
			return cfg, wrapDecodeErr(err)
		}
		if err := checkAPIVersion(cfg.APIVersion); err != nil {
			return cfg, err
		}
	}

	if err := applyEnv(&cfg, os.Environ()); err != nil {
		return cfg, err
	}
	if err := Validate(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// checkAPIVersion rejects a mismatched apiVersion with a typed error.
func checkAPIVersion(got string) error {
	if got != APIVersion {
		return fmt.Errorf("%w: got %q, want %q", ErrAPIVersion, got, APIVersion)
	}
	return nil
}

// wrapDecodeErr converts strict-YAML failures into tagged typed errors.
func wrapDecodeErr(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) && strings.Contains(te.Error(), "field ") && strings.Contains(te.Error(), "not found") {
		return fmt.Errorf("%w: %w", ErrUnknownField, err)
	}
	return fmt.Errorf("config: parse: %w", err)
}

// envPrefix restricts environment overrides to TOP's own namespace.
const envPrefix = "TOP_"

// envOverrides maps TOP_* env var names (without prefix) to struct fields.
// Only scalar leaves are overridable; everything else is file/CLI territory.
func applyEnv(cfg *Config, environ []string) error {
	var errs []FieldError
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, envPrefix) {
			continue
		}
		switch strings.TrimPrefix(name, envPrefix) {
		case "CONTROLLER_POLL_INTERVAL_SECONDS":
			if err := setInt(&cfg.Controller.PollIntervalSeconds, value); err != nil {
				errs = append(errs, FieldError{Field: "controller.pollIntervalSeconds", Err: err})
			}
		case "CONTROLLER_RECONCILE_INTERVAL_SECONDS":
			if err := setInt(&cfg.Controller.ReconcileIntervalSeconds, value); err != nil {
				errs = append(errs, FieldError{Field: "controller.reconcileIntervalSeconds", Err: err})
			}
		case "GIT_URL":
			cfg.Git.URL = value
		case "GIT_BRANCH":
			cfg.Git.Branch = value
		case "DATADIR", "DATA_DIR":
			cfg.DataDir = value
		case "SECRETSPROVIDER_KIND":
			if err := setKind(&cfg.SecretsProvider.Kind, value); err != nil {
				errs = append(errs, FieldError{Field: "secretsProvider.kind", Err: err})
			}
		case "LOG_LEVEL":
			cfg.LogLevel = value
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

func setInt(dst *int, value string) error {
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("TOP_ env override %q: not an integer", value)
	}
	*dst = v
	return nil
}

func setKind(dst *SecretsProviderKind, value string) error {
	k := SecretsProviderKind(strings.TrimSpace(value))
	switch k {
	case SecretsProviderHostfile, SecretsProviderVault:
		*dst = k
		return nil
	default:
		return fmt.Errorf("unknown secretsProvider kind %q (known: hostfile, vault)", k)
	}
}

// Validate checks a (usually defaulted or loaded) Config. Intervals must be
// positive, the reserved list must keep TOP's immutable namespaces, the
// policy order must be exactly core→observability→default, and DataDir must
// be absolute (overrideable in tests/CI, never relative in production).
func Validate(cfg Config) error {
	var errs []FieldError

	add := func(field string, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Err: fmt.Errorf(format, args...)})
	}

	if cfg.APIVersion != APIVersion {
		errs = append(errs, FieldError{
			Field: "apiVersion",
			Err:   fmt.Errorf("%w: got %q, want %q", ErrAPIVersion, cfg.APIVersion, APIVersion),
		})
	}
	if cfg.Controller.PollIntervalSeconds <= 0 {
		add("controller.pollIntervalSeconds", "must be > 0 (got %d)", cfg.Controller.PollIntervalSeconds)
	}
	if cfg.Controller.ReconcileIntervalSeconds <= 0 {
		add("controller.reconcileIntervalSeconds", "must be > 0 (got %d)", cfg.Controller.ReconcileIntervalSeconds)
	}
	switch cfg.SecretsProvider.Kind {
	case SecretsProviderHostfile, SecretsProviderVault:
		// vault is known-but-unimplemented; failing at *resolution* time
		// (ErrUnimplemented) is ADR-0008 behavior, not a config error.
	default:
		add("secretsProvider.kind", "unknown kind %q (known: hostfile, vault)", cfg.SecretsProvider.Kind)
	}

	// Reserved namespaces: immutable members must stay reserved.
	seen := make(map[string]bool, len(cfg.Namespaces.Reserved))
	for _, ns := range cfg.Namespaces.Reserved {
		if ns == "" {
			add("namespaces.reserved", "empty namespace entry")
			continue
		}
		if seen[ns] {
			add("namespaces.reserved", "duplicate entry %q", ns)
		}
		seen[ns] = true
	}
	for _, must := range ReservedNamespaces {
		if !seen[must] {
			add("namespaces.reserved", "immutable namespace %q may not be un-reserved", must)
		}
	}

	orderSeen := make(map[string]bool)
	for _, ns := range cfg.Namespaces.Order {
		if orderSeen[ns] {
			add("namespaces.order", "duplicate entry %q", ns)
		}
		orderSeen[ns] = true
	}
	// Policy order is fixed: core → observability → default.
	want := NamespacePolicyOrder
	if len(cfg.Namespaces.Order) != len(want) {
		add("namespaces.order", "must be exactly [core observability default] (got %v)", cfg.Namespaces.Order)
	} else {
		for i := range want {
			if cfg.Namespaces.Order[i] != want[i] {
				add("namespaces.order", "fixed policy order core→observability→default violated (got %v)", cfg.Namespaces.Order)
				break
			}
		}
	}

	if !strings.HasPrefix(cfg.DataDir, "/") {
		add("dataDir", "must be an absolute path (got %q)", cfg.DataDir)
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("logLevel", "unknown level %q (known: debug, info, warn, error)", cfg.LogLevel)
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// SecretsDir returns the directory of the hostfile secret store beneath the
// configured data directory (ADR-0008: /var/lib/top/secrets by default).
func (c Config) SecretsDir() string { return c.DataDir + "/secrets" }
