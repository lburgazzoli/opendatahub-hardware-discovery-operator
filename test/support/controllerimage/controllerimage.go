/*
Copyright 2026 lburgazzoli.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controllerimage resolves and validates the published
// run-scoped controller image inputs of the e2e registry delivery.
//
// The discovery service image is never built or imported locally: the
// chart-referenced Deployment pulls it from the ttl.sh ephemeral
// registry, and containerd resolves the child for the node's native
// architecture. The inputs are therefore mandatory separate whole
// values with an exact grammar shared with the publishing orchestration:
//
//	E2E_CONTROLLER_IMAGE_REPOSITORY=ttl.sh/hw-discovery-<uuid>
//	E2E_CONTROLLER_IMAGE_TAG=6h
//
// where <uuid> is a lowercase canonical UUIDv4 (8-4-4-4-12 hex, version
// nibble 4, variant nibble 8|9|a|b) and 6h is ttl.sh's approved
// retention tag. A combined repository:tag reference is never split, and
// no localhost default is ever invented: missing or invalid inputs are
// deterministic failures that occur before any cluster is created.
package controllerimage

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Environment keys carrying the mandatory separate whole image inputs.
// They are set by the registry orchestration (make test-e2e / make demo)
// and must be given explicitly by any direct Go invocation.
const (
	// RepositoryEnvKey carries the whole repository (host and path, no
	// tag).
	RepositoryEnvKey = "E2E_CONTROLLER_IMAGE_REPOSITORY"

	// TagEnvKey carries the whole tag.
	TagEnvKey = "E2E_CONTROLLER_IMAGE_TAG"
)

// ApprovedTag is the only accepted tag: ttl.sh's 6h retention window.
const ApprovedTag = "6h"

// repositoryPrefix is the approved repository grammar's fixed host and
// name prefix; the remainder must be a lowercase canonical UUIDv4.
const repositoryPrefix = "ttl.sh/hw-discovery-"

// uuidV4Pattern matches exactly a lowercase canonical UUIDv4: 8-4-4-4-12
// lowercase hex digits, version nibble 4 and variant nibble 8|9|a|b.
var uuidV4Pattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Reference is one resolved controller image input pair: the whole
// repository (never a combined reference) and the approved tag.
type Reference struct {
	// Repository is the exact published repository, e.g.
	// ttl.sh/hw-discovery-<uuid>.
	Repository string

	// Tag is the exact approved tag (6h).
	Tag string
}

// Image returns the combined image reference repository:tag. It is
// intended for display and for asserting the deployed reference; the
// Helm values always receive the separate whole fields.
func (r Reference) Image() string {
	return r.Repository + ":" + r.Tag
}

// remedy directs the user to the supported input paths: the make
// orchestration that publishes the image, or the explicit env inputs.
const remedy = "run `make test-e2e` or `make demo` (which publish the run-scoped image), or set " +
	RepositoryEnvKey + "=ttl.sh/hw-discovery-<uuid> and " + TagEnvKey + "=" + ApprovedTag +
	" explicitly to the published values"

// Validate checks one input pair against the exact approved grammar.
// Values are whitespace-trimmed first; everything else must match
// exactly. The returned errors carry the remedy, so callers can fail
// deterministically with an actionable message before creating any
// cluster.
func Validate(ref Reference) error {
	repository := strings.TrimSpace(ref.Repository)
	tag := strings.TrimSpace(ref.Tag)

	if repository == "" {
		return fmt.Errorf("%s must be set to the published repository (ttl.sh/hw-discovery-<lowercase uuidv4>): %s",
			RepositoryEnvKey, remedy)
	}
	if strings.Contains(repository, ":") {
		return fmt.Errorf("%s must be a whole repository without a tag separator, got %q: "+
			"repository and tag are separate values and are never split; %s",
			RepositoryEnvKey, repository, remedy)
	}
	if !strings.HasPrefix(repository, repositoryPrefix) {
		return fmt.Errorf("%s must be %s<lowercase uuidv4>, got %q: the repository host and name are fixed "+
			"by the approved run-scoped grammar (local or other-host images are not consumed); %s",
			RepositoryEnvKey, repositoryPrefix, repository, remedy)
	}

	uuid := strings.TrimPrefix(repository, repositoryPrefix)
	if !uuidV4Pattern.MatchString(uuid) {
		return fmt.Errorf("%s must be %s<lowercase uuidv4>, got %q: the suffix must be a canonical lowercase "+
			"UUIDv4 (8-4-4-4-12 hex, version nibble 4, variant nibble 8|9|a|b); %s",
			RepositoryEnvKey, repositoryPrefix, repository, remedy)
	}

	if tag == "" {
		return fmt.Errorf("%s must be set to the approved tag %q: %s", TagEnvKey, ApprovedTag, remedy)
	}
	if tag != ApprovedTag {
		return fmt.Errorf("%s must be exactly the approved tag %q, got %q; %s",
			TagEnvKey, ApprovedTag, tag, remedy)
	}

	return nil
}

// Resolve reads the mandatory inputs through getenv — a seam so unit
// tests never touch the process environment — and validates them. The
// returned reference carries the exact (whitespace-trimmed) published
// values, never defaults, never a split of a combined reference.
func Resolve(getenv func(string) string) (Reference, error) {
	if getenv == nil {
		return Reference{}, errors.New("controllerimage: getenv must not be nil")
	}

	ref := Reference{
		Repository: getenv(RepositoryEnvKey),
		Tag:        getenv(TagEnvKey),
	}
	if err := Validate(ref); err != nil {
		return Reference{}, err
	}
	return Reference{
		Repository: strings.TrimSpace(ref.Repository),
		Tag:        strings.TrimSpace(ref.Tag),
	}, nil
}

// ResolveEnv reads the mandatory inputs from the process environment and
// validates them.
func ResolveEnv() (Reference, error) {
	return Resolve(os.Getenv)
}
