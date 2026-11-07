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

package discovery_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/pkg/discovery"
)

func TestLocatorContract(t *testing.T) {
	g := NewWithT(t)

	dra := draLocator()
	classic := classicLocator()

	g.Expect(dra.Kind()).To(Equal(draSourceID))
	g.Expect(classic.Kind()).To(Equal(classicSourceID))
	g.Expect(dra.Validate()).To(Succeed())
	g.Expect(classic.Validate()).To(Succeed())

	// Equal is deep within a kind and false across kinds.
	g.Expect(dra.Equal(draLocator())).To(BeTrue())
	g.Expect(classic.Equal(classicLocator())).To(BeTrue())
	g.Expect(dra.Equal(classic)).To(BeFalse())
	g.Expect(classic.Equal(dra)).To(BeFalse())

	// A locator of the same kind but different provenance differs.
	changedRef := fakeLocator{kind: draSourceID, ref: nvidiaDriver + "/" + otherNode}
	g.Expect(dra.Equal(changedRef)).To(BeFalse())

	// An invalid locator fails Validate with its classified cause.
	invalid := fakeLocator{kind: draSourceID, ref: nodeName, invalid: discovery.ErrIncompleteLocator}
	err := invalid.Validate()
	g.Expect(err).To(MatchError(discovery.ErrIncompleteLocator))

	// Caller-owned values are not mutated by the comparison.
	original := dra.(fakeLocator)
	_ = dra.Equal(classic)
	g.Expect(dra.(fakeLocator)).To(Equal(original))
}

func TestSourceRef(t *testing.T) {
	g := NewWithT(t)

	draRef, err := discovery.SourceRef(draSourceID, draLocator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(draRef).To(Equal("dra/" + nvidiaDriver + "/" + nodeName))

	classicRef, err := discovery.SourceRef(classicSourceID, classicLocator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classicRef).To(Equal("classic/" + nodeName))

	// A missing locator is rejected, never rendered as a partial ref.
	_, err = discovery.SourceRef(draSourceID, nil)
	g.Expect(err).To(MatchError(discovery.ErrNoLocator))

	// An invalid locator is rejected with its classified cause.
	invalid := fakeLocator{kind: draSourceID, ref: nodeName, invalid: discovery.ErrIncompleteLocator}
	_, err = discovery.SourceRef(draSourceID, invalid)
	g.Expect(err).To(MatchError(discovery.ErrIncompleteLocator))
}
