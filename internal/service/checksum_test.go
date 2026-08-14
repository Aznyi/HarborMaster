package service_test

import (
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

func checksumFixture() []domain.ContainerDetail {
	return []domain.ContainerDetail{
		{
			Overview: domain.ContainerSummary{
				ID: "c1", Name: "web", Image: domain.ParseImageRef("nginx:1.27"),
				ImageID: "sha256:img1", State: domain.StateRunning, Health: domain.HealthHealthy,
				RestartPolicy: domain.RestartPolicy{Name: "always"},
				Compose:       domain.ComposeMetadata{Managed: true, Project: "shop", Service: "web"},
				Present:       true,
			},
			Ports: []domain.Port{
				{ContainerPort: 80, Protocol: "tcp", HostPort: 8080, Published: true},
				{ContainerPort: 443, Protocol: "tcp", Published: false},
			},
			Labels: []domain.Label{
				{Key: "b", Value: "2"},
				{Key: "a", Value: "1"},
			},
			Environment: []domain.EnvVar{
				{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
				fixtureSecret("DB_PASSWORD", "hunter2"),
			},
			Networks: []domain.NetworkAttachment{
				{NetworkName: "frontend", IPv4Address: "172.20.0.2", Aliases: []string{"z", "a"}},
			},
			Mounts: []domain.Mount{
				{Type: domain.MountTypeBind, Source: "/srv", Destination: "/data"},
			},
			Resources: domain.Resources{CPUShares: 512, MemoryBytes: 1024},
			Security:  domain.Security{ReadonlyRootfs: true, CapDrop: []string{"ALL", "NET_RAW"}},
			Logging:   domain.Logging{Driver: "json-file"},
		},
		{
			Overview: domain.ContainerSummary{
				ID: "c2", Name: "db", Image: domain.ParseImageRef("postgres:16"),
				State: domain.StateRunning, Health: domain.HealthNone, Present: true,
			},
		},
	}
}

func fixtureImages() []domain.Image {
	return []domain.Image{{ID: "sha256:img1"}, {ID: "sha256:img2"}}
}

func TestChecksumIsStableForIdenticalInventory(t *testing.T) {
	first := service.InventoryChecksum(checksumFixture(), fixtureImages(), nil, nil)

	for i := 0; i < 10; i++ {
		next := service.InventoryChecksum(checksumFixture(), fixtureImages(), nil, nil)
		if next != first {
			t.Fatalf("checksum changed between identical inventories:\n%s\n%s", first, next)
		}
	}
	if len(first) != 64 {
		t.Errorf("checksum length = %d, want a 64-character SHA-256 hex digest", len(first))
	}
}

// Container order is an artefact of concurrent inspection, not a property of
// the inventory.
func TestChecksumIgnoresContainerOrder(t *testing.T) {
	forward := checksumFixture()
	reversed := []domain.ContainerDetail{forward[1], forward[0]}

	if service.InventoryChecksum(forward, nil, nil, nil) !=
		service.InventoryChecksum(reversed, nil, nil, nil) {
		t.Error("reordering containers changed the checksum")
	}
}

// Sets whose order carries no meaning must not affect the checksum.
func TestChecksumIgnoresOrderOfUnorderedCollections(t *testing.T) {
	base := checksumFixture()
	shuffled := checksumFixture()

	shuffled[0].Ports = []domain.Port{shuffled[0].Ports[1], shuffled[0].Ports[0]}
	shuffled[0].Labels = []domain.Label{shuffled[0].Labels[1], shuffled[0].Labels[0]}
	shuffled[0].Security.CapDrop = []string{"NET_RAW", "ALL"}
	shuffled[0].Networks[0].Aliases = []string{"a", "z"}

	if service.InventoryChecksum(base, nil, nil, nil) !=
		service.InventoryChecksum(shuffled, nil, nil, nil) {
		t.Error("reordering ports, labels, capabilities, or aliases changed the checksum")
	}
}

// Environment order IS meaningful to some programs, so it must count.
func TestChecksumRespectsEnvironmentOrder(t *testing.T) {
	base := checksumFixture()
	swapped := checksumFixture()
	swapped[0].Environment = []domain.EnvVar{
		swapped[0].Environment[1], swapped[0].Environment[0],
	}

	if service.InventoryChecksum(base, nil, nil, nil) ==
		service.InventoryChecksum(swapped, nil, nil, nil) {
		t.Error("environment order is semantically meaningful and should affect the checksum")
	}
}

// Volatile observation metadata must not churn the checksum, or it would
// change on every refresh and be useless for change detection.
func TestChecksumIgnoresVolatileFields(t *testing.T) {
	base := checksumFixture()

	volatile := checksumFixture()
	started := time.Now()
	volatile[0].Overview.Status = "Up 3 minutes"
	volatile[0].Overview.CreatedAt = time.Now()
	volatile[0].Overview.StartedAt = &started
	volatile[0].Overview.LastSeenAt = time.Now()
	volatile[0].Overview.FirstSeenAt = time.Now()
	volatile[0].Overview.Generation = 42
	volatile[0].Overview.WarningCount = 3
	volatile[0].State.HealthLog = []domain.HealthLogEntry{{ExitCode: 0}}

	if service.InventoryChecksum(base, nil, nil, nil) !=
		service.InventoryChecksum(volatile, nil, nil, nil) {
		t.Error("volatile observation metadata changed the checksum")
	}
}

// The point of hashing raw values: a changed secret is a changed inventory,
// even though the masked value that reaches the API is identical.
func TestChecksumDetectsChangedSecretValues(t *testing.T) {
	base := checksumFixture()

	rotated := checksumFixture()
	setFixtureSecret(rotated[0].Environment, "DB_PASSWORD", "a-different-password")
	// The masked value is unchanged, exactly as the API would report it.
	if rotated[0].Environment[1].Value != domain.MaskedValue {
		t.Fatal("fixture should keep the masked value identical")
	}

	if service.InventoryChecksum(base, nil, nil, nil) ==
		service.InventoryChecksum(rotated, nil, nil, nil) {
		t.Error("a rotated secret must change the checksum")
	}
}

// ...and the checksum must not simply be the secret in another form.
func TestChecksumDoesNotContainSecretValues(t *testing.T) {
	checksum := service.InventoryChecksum(checksumFixture(), nil, nil, nil)

	if len(checksum) != 64 {
		t.Fatalf("unexpected checksum shape: %q", checksum)
	}
	for _, secret := range []string{"hunter2", domain.MaskedValue} {
		if contains(checksum, secret) {
			t.Errorf("checksum contains %q", secret)
		}
	}
}

func TestChecksumDetectsMeaningfulChanges(t *testing.T) {
	base := service.InventoryChecksum(checksumFixture(), fixtureImages(), nil, nil)

	tests := map[string]func([]domain.ContainerDetail) []domain.ContainerDetail{
		"state changed": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Overview.State = domain.StateExited
			return d
		},
		"health changed": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Overview.Health = domain.HealthUnhealthy
			return d
		},
		"image changed": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Overview.Image = domain.ParseImageRef("nginx:1.28")
			return d
		},
		"port published": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Ports[1].Published = true
			return d
		},
		"label added": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Labels = append(d[0].Labels, domain.Label{Key: "new", Value: "x"})
			return d
		},
		"mount added": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Mounts = append(d[0].Mounts, domain.Mount{Destination: "/extra"})
			return d
		},
		"memory limit changed": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Resources.MemoryBytes = 2048
			return d
		},
		"privileged enabled": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			d[0].Security.Privileged = true
			return d
		},
		"container removed": func(d []domain.ContainerDetail) []domain.ContainerDetail {
			return d[:1]
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if got := service.InventoryChecksum(mutate(checksumFixture()), fixtureImages(), nil, nil); got == base {
				t.Error("the change did not affect the checksum")
			}
		})
	}
}

// Nil and zero are different configurations, and the checksum must say so.
func TestChecksumDistinguishesUnsetFromZero(t *testing.T) {
	unset := checksumFixture()

	zero := checksumFixture()
	limit := int64(0)
	zero[0].Resources.PidsLimit = &limit

	if service.InventoryChecksum(unset, nil, nil, nil) ==
		service.InventoryChecksum(zero, nil, nil, nil) {
		t.Error("an unset PidsLimit and one set to 0 must hash differently")
	}
}

// Length prefixing prevents field-boundary collisions.
func TestChecksumIsNotAmbiguousAcrossFieldBoundaries(t *testing.T) {
	first := []domain.ContainerDetail{{
		Overview: domain.ContainerSummary{ID: "c", Name: "ab", Status: "x"},
	}}
	second := []domain.ContainerDetail{{
		Overview: domain.ContainerSummary{ID: "c", Name: "a", Status: "bx"},
	}}

	if service.InventoryChecksum(first, nil, nil, nil) ==
		service.InventoryChecksum(second, nil, nil, nil) {
		t.Error("adjacent field values collided; encoding is ambiguous")
	}
}

func TestChecksumCoversCatalogEntities(t *testing.T) {
	containers := checksumFixture()
	base := service.InventoryChecksum(containers, fixtureImages(), nil, nil)

	withNetwork := service.InventoryChecksum(containers, fixtureImages(),
		[]domain.Network{{ID: "n1", Name: "bridge"}}, nil)
	if base == withNetwork {
		t.Error("adding a network did not change the checksum")
	}

	withVolume := service.InventoryChecksum(containers, fixtureImages(), nil,
		[]domain.Volume{{Name: "data"}})
	if base == withVolume {
		t.Error("adding a volume did not change the checksum")
	}
}

func TestChecksumOfEmptyInventoryIsStable(t *testing.T) {
	first := service.InventoryChecksum(nil, nil, nil, nil)
	second := service.InventoryChecksum([]domain.ContainerDetail{}, []domain.Image{}, nil, nil)

	if first != second {
		t.Error("nil and empty inventories should hash identically")
	}
	if first == "" {
		t.Error("an empty inventory should still have a checksum")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
