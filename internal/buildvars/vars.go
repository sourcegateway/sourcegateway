// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package buildvars

var (
	buildVersion = "dev"
	buildDate    = "unknown"
	commitHash   = "unknown"
	commitDate   = "unknown"
	commitBranch = "unknown"
)

// Version returns the release version injected at build time.
func Version() string { return buildVersion }

// BuildDate returns the build timestamp injected at build time.
func BuildDate() string { return buildDate }

// CommitHash returns the source revision injected at build time.
func CommitHash() string { return commitHash }

// CommitDate returns the revision timestamp injected at build time.
func CommitDate() string { return commitDate }

// CommitBranch returns the source branch injected at build time.
func CommitBranch() string { return commitBranch }
