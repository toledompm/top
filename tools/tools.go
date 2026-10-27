//go:build tools

// Package tools is not real code: it exists to keep the project's pinned
// dependency set in go.mod (go mod tidy would otherwise drop modules no
// package imports yet). Task packages land in Milestone 1 (docs/tasks/backlog.md
// T-102..T-108); when they import these modules this file can shrink or go.
package tools

import (
	_ "github.com/cenkalti/backoff/v5"
	_ "github.com/google/uuid"
	_ "github.com/pressly/goose/v3"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "go.yaml.in/yaml/v3"
	_ "golang.org/x/sync/errgroup"
	_ "modernc.org/sqlite"
)
