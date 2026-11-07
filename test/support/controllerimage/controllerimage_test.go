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

package controllerimage

import (
	"testing"

	. "github.com/onsi/gomega"
)

// validUUID is a canonical lowercase UUIDv4 used by the valid cases.
const validUUID = "3f2a1b0c-9d4e-4f5a-8b6c-1d2e3f4a5b6c"

// wantUUIDGrammar is the shared rejection substring of every malformed
// repository case.
const wantUUIDGrammar = "canonical lowercase UUIDv4"

func validRepository() string {
	return repositoryPrefix + validUUID
}

func TestValidateAcceptsExactApprovedInputs(t *testing.T) {
	g := NewWithT(t)

	ref := Reference{Repository: validRepository(), Tag: ApprovedTag}
	g.Expect(Validate(ref)).To(Succeed(),
		"the exact lowercase UUIDv4 repository and the approved tag must be accepted")
}

func TestValidateRejectsInputs(t *testing.T) {
	wrongHost := "docker.io/hw-discovery-" + validUUID
	localhostFallback := "localhost/opendatahub-hardware-discovery-operator"
	combined := validRepository() + ":" + ApprovedTag
	malformedUUID := repositoryPrefix + "not-a-uuid"
	shortUUID := repositoryPrefix + "3f2a1b0c-9d4e-4f5a-8b6c-1d2e3f4a"
	nonHexUUID := repositoryPrefix + "3f2a1b0c-9d4e-4f5a-8b6c-1d2e3f4a5b6z"
	nonV4UUID := repositoryPrefix + "3f2a1b0c-9d4e-5f5a-8b6c-1d2e3f4a5b6c"
	wrongVariant := repositoryPrefix + "3f2a1b0c-9d4e-4f5a-cb6c-1d2e3f4a5b6c"
	uppercaseUUID := repositoryPrefix + "3F2A1B0C-9D4E-4F5A-8B6C-1D2E3F4A5B6C"

	table := []struct {
		name       string
		repository string
		tag        string
		want       string
	}{
		{
			name:       "absent repository",
			repository: "",
			tag:        ApprovedTag,
			want:       RepositoryEnvKey + " must be set",
		},
		{
			name:       "whitespace-only repository",
			repository: " \t ",
			tag:        ApprovedTag,
			want:       RepositoryEnvKey + " must be set",
		},
		{
			name:       "absent tag",
			repository: validRepository(),
			tag:        "",
			want:       TagEnvKey + " must be set",
		},
		{
			name:       "whitespace-only tag",
			repository: validRepository(),
			tag:        "  ",
			want:       TagEnvKey + " must be set",
		},
		{
			name:       "fixed localhost fallback is rejected",
			repository: localhostFallback,
			tag:        ApprovedTag,
			want:       "local or other-host images are not consumed",
		},
		{
			name:       "wrong host is rejected",
			repository: wrongHost,
			tag:        ApprovedTag,
			want:       "must be " + repositoryPrefix,
		},
		{
			name:       "missing name prefix is rejected",
			repository: "ttl.sh/other-" + validUUID,
			tag:        ApprovedTag,
			want:       "must be " + repositoryPrefix,
		},
		{
			name:       "combined repository and tag is rejected, never split",
			repository: combined,
			tag:        ApprovedTag,
			want:       "without a tag separator",
		},
		{
			name:       "malformed uuid is rejected",
			repository: malformedUUID,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "truncated uuid is rejected",
			repository: shortUUID,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "non-hex uuid is rejected",
			repository: nonHexUUID,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "non-v4 uuid is rejected",
			repository: nonV4UUID,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "wrong variant nibble is rejected",
			repository: wrongVariant,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "uppercase uuid is rejected",
			repository: uppercaseUUID,
			tag:        ApprovedTag,
			want:       wantUUIDGrammar,
		},
		{
			name:       "wrong tag is rejected",
			repository: validRepository(),
			tag:        "latest",
			want:       "must be exactly the approved tag",
		},
		{
			name:       "wrong ttl duration tag is rejected",
			repository: validRepository(),
			tag:        "24h",
			want:       "must be exactly the approved tag",
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			err := Validate(Reference{Repository: entry.repository, Tag: entry.tag})
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
			// Every rejection carries the remedy.
			g.Expect(err.Error()).To(ContainSubstring("`make test-e2e`"))
		})
	}
}

func TestResolveReturnsPublishedValuesUnchanged(t *testing.T) {
	g := NewWithT(t)

	repository := validRepository()
	env := map[string]string{
		RepositoryEnvKey: repository,
		TagEnvKey:        ApprovedTag,
	}

	ref, err := Resolve(func(key string) string { return env[key] })
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ref.Repository).To(Equal(repository), "the whole repository must pass through unchanged")
	g.Expect(ref.Tag).To(Equal(ApprovedTag), "the whole tag must pass through unchanged")
	g.Expect(ref.Image()).To(Equal(repository + ":" + ApprovedTag))
}

func TestResolveFailsBeforeReturningPartialValues(t *testing.T) {
	g := NewWithT(t)

	env := map[string]string{
		RepositoryEnvKey: "localhost/opendatahub-hardware-discovery-operator",
		TagEnvKey:        "e2e",
	}

	ref, err := Resolve(func(key string) string { return env[key] })
	g.Expect(err).To(HaveOccurred())
	g.Expect(ref).To(Equal(Reference{}), "a rejected input must not yield a usable reference")
}

func TestResolveRejectsMissingEnvInputs(t *testing.T) {
	g := NewWithT(t)

	// Nothing is set: no localhost default may be invented.
	ref, err := Resolve(func(string) string { return "" })
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(RepositoryEnvKey + " must be set"))
	g.Expect(ref).To(Equal(Reference{}))
}

func TestResolveRejectsNilGetenv(t *testing.T) {
	g := NewWithT(t)

	_, err := Resolve(nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getenv must not be nil"))
}
