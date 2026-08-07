package domain

import (
	"strings"
	"testing"
)

// Self-identity tests.
//
// The property under test is asymmetric, and both directions matter:
//
//   - Every signal that CAN identify HarborMaster must do so, independently, so
//     one failed probe does not open the door.
//   - No signal that is ABSENT may identify anything, so a failed probe does not
//     close every door.

const selfID = "1111111111111111111111111111111111111111111111111111111111111111"
const otherID = "2222222222222222222222222222222222222222222222222222222222222222"

func TestEverySignalIdentifiesHarborMasterIndependently(t *testing.T) {
	cases := []struct {
		name     string
		identity SelfIdentity
		target   SelfTarget
	}{
		{
			"by container id",
			SelfIdentity{ContainerID: selfID},
			SelfTarget{ContainerID: selfID, ContainerName: "anything"},
		},
		{
			"by container id, case insensitive",
			SelfIdentity{ContainerID: strings.ToUpper(selfID)},
			SelfTarget{ContainerID: selfID},
		},
		{
			// The path pauses and selectors take: they are keyed on names.
			"by container name",
			SelfIdentity{ContainerName: "harbormaster"},
			SelfTarget{ContainerName: "harbormaster"},
		},
		{
			// Independent of any container identity: a second copy of
			// HarborMaster is still HarborMaster.
			"by image id",
			SelfIdentity{ImageID: "sha256:aaa"},
			SelfTarget{ContainerID: otherID, ImageID: "sha256:aaa"},
		},
		{
			"by image reference",
			SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"},
			SelfTarget{ContainerID: otherID, ImageRef: "ghcr.io/aznyi/harbormaster:0.9"},
		},
		{
			// Two versions of HarborMaster are both HarborMaster, and neither
			// may update the other.
			"by image repository, across tags",
			SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"},
			SelfTarget{ImageRef: "ghcr.io/aznyi/harbormaster:0.10"},
		},
		{
			"by image repository, tag against digest",
			SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"},
			SelfTarget{ImageRef: "ghcr.io/aznyi/harbormaster@sha256:" + strings.Repeat("a", 64)},
		},
		{
			// A registry port must not be mistaken for a tag separator.
			"by image repository, with a registry port",
			SelfIdentity{ImageRef: "registry.example:5000/harbormaster:0.9"},
			SelfTarget{ImageRef: "registry.example:5000/harbormaster:0.10"},
		},
		{
			"by label",
			SelfIdentity{},
			SelfTarget{Labels: map[string]string{LabelSelfIdentity: "true"}},
		},
		{
			"by label, alternative spellings",
			SelfIdentity{},
			SelfTarget{Labels: map[string]string{LabelSelfIdentity: "YES"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, why := tc.identity.SelfMatch(tc.target)
			if !matched {
				t.Fatal("this signal must identify HarborMaster on its own")
			}
			if why == "" {
				t.Fatal("a refusal must say why, or an operator cannot act on it")
			}
		})
	}
}

func TestADifferentContainerIsNotHarborMaster(t *testing.T) {
	identity := SelfIdentity{
		ContainerID:   selfID,
		ContainerName: "harbormaster",
		ImageRef:      "ghcr.io/aznyi/harbormaster:0.9",
		ImageID:       "sha256:aaa",
	}

	cases := []SelfTarget{
		{ContainerID: otherID, ContainerName: "web", ImageRef: "nginx:1.27", ImageID: "sha256:bbb"},
		// A different repository at the same registry.
		{ImageRef: "ghcr.io/aznyi/something-else:0.9"},
		// A similarly named container.
		{ContainerName: "harbormaster-proxy"},
		// The label explicitly false.
		{ContainerName: "web", Labels: map[string]string{LabelSelfIdentity: "false"}},
		// A label value this build does not understand. The safe reading is
		// "not HarborMaster": every other signal still applies.
		{ContainerName: "web", Labels: map[string]string{LabelSelfIdentity: "perhaps"}},
	}

	for _, target := range cases {
		if matched, _ := identity.SelfMatch(target); matched {
			t.Errorf("%+v was wrongly identified as HarborMaster", target)
		}
	}
}

func TestALabelCannotUnsetTheIdentity(t *testing.T) {
	// The asymmetry: a label may say "I am HarborMaster" and be believed,
	// because believing it only ever refuses an update. It may not say "I am
	// NOT HarborMaster" and be believed, because that would let anyone who can
	// run `docker run` opt the real HarborMaster back into being updated.
	identity := SelfIdentity{ContainerID: selfID}

	matched, _ := identity.SelfMatch(SelfTarget{
		ContainerID: selfID,
		Labels:      map[string]string{LabelSelfIdentity: "false"},
	})
	if !matched {
		t.Fatal("a label must never be able to clear HarborMaster's own identity")
	}
}

func TestKnownReportsWhetherAnythingWasEstablished(t *testing.T) {
	if (SelfIdentity{}).Known() {
		t.Fatal("an empty identity establishes nothing")
	}
	if !(SelfIdentity{ContainerID: selfID}).Known() {
		t.Fatal("an id is something")
	}
	if !(SelfIdentity{ImageRef: "x"}).Known() {
		t.Fatal("an image is something")
	}
	// A source and a detail alone are not knowledge: they describe a failed
	// detection, which must not read as a successful one.
	if (SelfIdentity{Source: SelfSourceNone, Detail: "could not tell"}).Known() {
		t.Fatal("a failure detail is not an identity")
	}
}

func TestDescribeSaysWhatItKnows(t *testing.T) {
	cases := map[string]SelfIdentity{
		"HarborMaster is running as the container harbormaster": {
			ContainerName: "harbormaster",
		},
		"HarborMaster could not determine which container it is running in": {},
	}
	for want, identity := range cases {
		if got := identity.Describe(); got != want {
			t.Errorf("describe = %q, want %q", got, want)
		}
	}

	// The partial case names the image and says the id is unknown, rather than
	// claiming more than it has.
	partial := SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"}.Describe()
	if !strings.Contains(partial, "could not determine which container") {
		t.Fatalf("a partial identity must say what it does not know: %q", partial)
	}
}

func TestRepositoryComparisonIsNotAPrefixMatch(t *testing.T) {
	// `harbormaster` and `harbormaster-agent` are different software. A prefix
	// comparison would exclude the second from ever being updated.
	identity := SelfIdentity{ImageRef: "ghcr.io/aznyi/harbormaster:0.9"}

	if matched, _ := identity.SelfMatch(SelfTarget{
		ImageRef: "ghcr.io/aznyi/harbormaster-agent:0.9",
	}); matched {
		t.Fatal("a different repository sharing a prefix is not HarborMaster")
	}
}
