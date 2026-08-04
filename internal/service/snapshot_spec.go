package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// BuildSpec converts an observed container into the canonical document.
//
// Volatile fields are not filtered out here -- they have no field to occupy in
// domain.SnapshotSpec at all. That is deliberate: filtering can be forgotten
// when a field is added, while a type with no such field cannot carry one.
//
// Sensitive values are read exactly once, hashed, and dropped. After this
// function returns, no plaintext secret from the container exists anywhere in
// the snapshot pipeline.
func BuildSpec(detail domain.ContainerDetail, image *domain.Image, hasher *Hasher) domain.SnapshotSpec {
	overview := detail.Overview

	spec := domain.SnapshotSpec{
		SpecVersion: domain.SnapshotSpecVersion,
		Identity: domain.SpecIdentity{
			ContainerID:   overview.ID,
			ContainerName: overview.Name,
		},
		Image:         buildSpecImage(overview, image),
		Process:       detail.Process,
		Environment:   buildSpecEnv(detail.Environment, hasher),
		Labels:        sortedLabels(detail.Labels),
		Ports:         sortedPorts(detail.Ports),
		Networks:      buildSpecNetworks(detail.Networks),
		Mounts:        buildSpecMounts(detail.Mounts),
		RestartPolicy: overview.RestartPolicy,
		HealthCheck:   detail.HealthCheck,
		Resources:     sortedResources(detail.Resources),
		Security:      sortedSecurity(detail.Security),
		Logging: domain.SpecLogging{
			Driver:  detail.Logging.Driver,
			Options: buildSpecEnv(detail.Logging.Options, hasher),
		},
		Compose:      detail.Compose,
		HarborMaster: detail.HarborMaster,
	}
	return spec
}

// MarshalSpec renders the canonical document and its checksum.
//
// The checksum covers the exact bytes persisted plus the secret digests, which
// are not in those bytes. Re-hashing a stored document alone therefore will not
// reproduce the checksum -- VerifyChecksum exists for that, and takes the
// digests it needs from the child rows.
//
// Two configurations that differ only in a secret's value produce identical
// document bytes and different checksums, which is exactly the intent: the
// checksum notices the change, the document never contains the secret.
func MarshalSpec(spec domain.SnapshotSpec) (blob []byte, checksum string, err error) {
	blob, err = json.Marshal(spec)
	if err != nil {
		return nil, "", fmt.Errorf("encode snapshot document: %w", err)
	}
	return blob, checksumOf(blob, spec), nil
}

// VerifyChecksum recomputes a stored snapshot's checksum.
//
// Used by tests and by the integrity check on read: a snapshot whose document
// no longer hashes to its recorded checksum has been altered, and altered
// evidence is worse than no evidence.
func VerifyChecksum(blob []byte, spec domain.SnapshotSpec, want string) bool {
	return checksumOf(blob, spec) == want
}

// checksumOf hashes the document bytes followed by the secret digests.
//
// Length-prefixed, like the inventory checksum, so that two different
// name/digest splits cannot produce the same byte stream.
func checksumOf(blob []byte, spec domain.SnapshotSpec) string {
	hash := sha256.New()
	_, _ = hash.Write(blob)

	writeSection(hash, "secrets")
	writeSecretDigests(hash, "env", spec.Environment)
	writeSecretDigests(hash, "logOption", spec.Logging.Options)

	return hex.EncodeToString(hash.Sum(nil))
}

// writeSecretDigests folds sensitive values into the checksum through their
// digests only.
//
// A sensitive value contributes hash(name, digest) rather than the value, so
// changing a password changes the checksum without the checksum being derived
// from, or reversible to, the password.
func writeSecretDigests(hash interface{ Write([]byte) (int, error) }, field string, vars []domain.SpecEnvVar) {
	for _, v := range vars {
		if !v.Sensitive() {
			continue
		}
		writeField(hash, field, v.Name+"="+string(v.DigestAlgorithm)+":"+v.DigestKeyID+":"+v.Digest)
	}
}

func buildSpecImage(overview domain.ContainerSummary, image *domain.Image) domain.SpecImage {
	spec := domain.SpecImage{
		Reference:  overview.Image.Raw,
		Repository: overview.Image.Repository,
		Tag:        overview.Image.Tag,
		Digest:     overview.Image.Digest,
		ImageID:    overview.ImageID,
	}
	if image == nil {
		return spec
	}

	spec.Architecture = image.Architecture
	spec.OS = image.OS
	// Sorted: the runtime reports these in no guaranteed order, and an ordering
	// change is not a configuration change.
	spec.RepoDigests = append([]string(nil), image.RepoDigests...)
	sort.Strings(spec.RepoDigests)
	return spec
}

// buildSpecEnv classifies and hashes an environment or option list.
//
// Order is PRESERVED. Environment order is semantically meaningful to some
// programs, so a reordering is a real configuration change and must be visible
// as one -- unlike ports or mounts, where order carries nothing.
func buildSpecEnv(vars []domain.EnvVar, hasher *Hasher) []domain.SpecEnvVar {
	out := make([]domain.SpecEnvVar, 0, len(vars))

	for _, v := range vars {
		entry := domain.SpecEnvVar{
			Name:        v.Name,
			Sensitivity: v.Sensitivity,
			Present:     true,
		}

		if !v.Sensitive() {
			entry.Value = v.Value
			out = append(out, entry)
			continue
		}

		// The one place a raw secret is read. It is hashed here and never
		// referenced again: entry carries the digest, not the value.
		digest := hasher.Digest(v.RawValue)
		entry.Length = digest.Length
		entry.Digest = digest.Digest
		entry.DigestAlgorithm = digest.Algorithm
		entry.DigestKeyID = digest.KeyID
		out = append(out, entry)
	}
	return out
}

func sortedLabels(labels []domain.Label) []domain.Label {
	out := append([]domain.Label(nil), labels...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if out == nil {
		out = []domain.Label{}
	}
	return out
}

func sortedPorts(ports []domain.Port) []domain.Port {
	out := append([]domain.Port(nil), ports...)
	domain.SortPorts(out)
	if out == nil {
		out = []domain.Port{}
	}
	return out
}

func buildSpecNetworks(attachments []domain.NetworkAttachment) []domain.SpecNetwork {
	ordered := append([]domain.NetworkAttachment(nil), attachments...)
	domain.SortNetworkAttachments(ordered)

	out := make([]domain.SpecNetwork, 0, len(ordered))
	for _, a := range ordered {
		aliases := append([]string(nil), a.Aliases...)
		sort.Strings(aliases)
		links := append([]string(nil), a.Links...)
		sort.Strings(links)

		out = append(out, domain.SpecNetwork{
			NetworkName: a.NetworkName,
			Aliases:     aliases,
			Links:       links,
		})
	}
	return out
}

func buildSpecMounts(mounts []domain.Mount) []domain.SpecMount {
	ordered := append([]domain.Mount(nil), mounts...)
	domain.SortMounts(ordered)

	out := make([]domain.SpecMount, 0, len(ordered))
	for _, m := range ordered {
		out = append(out, domain.SpecMount{
			Type:         m.Type,
			Source:       m.Source,
			Destination:  m.Destination,
			ReadOnly:     m.ReadOnly,
			Propagation:  m.Propagation,
			VolumeName:   m.VolumeName,
			Driver:       m.Driver,
			TmpfsOptions: m.TmpfsOptions,
		})
	}
	return out
}

// sortedResources normalises the one order-insensitive list in Resources.
func sortedResources(r domain.Resources) domain.Resources {
	out := r
	out.Ulimits = append([]domain.Ulimit(nil), r.Ulimits...)
	sort.SliceStable(out.Ulimits, func(i, j int) bool { return out.Ulimits[i].Name < out.Ulimits[j].Name })
	return out
}

// sortedSecurity normalises every set-like list in Security.
//
// Capabilities, security options, and group memberships are sets: the runtime
// reports them in whatever order it stored them, and a reordering is not a
// change in posture.
func sortedSecurity(s domain.Security) domain.Security {
	out := s

	out.CapAdd = sortedCopy(s.CapAdd)
	out.CapDrop = sortedCopy(s.CapDrop)
	out.SecurityOpt = sortedCopy(s.SecurityOpt)
	out.DeviceCgroupRules = sortedCopy(s.DeviceCgroupRules)
	out.GroupAdd = sortedCopy(s.GroupAdd)

	out.Devices = append([]domain.Device(nil), s.Devices...)
	sort.SliceStable(out.Devices, func(i, j int) bool {
		if out.Devices[i].PathInContainer != out.Devices[j].PathInContainer {
			return out.Devices[i].PathInContainer < out.Devices[j].PathInContainer
		}
		return out.Devices[i].PathOnHost < out.Devices[j].PathOnHost
	})

	out.DeviceRequests = append([]domain.DeviceRequest(nil), s.DeviceRequests...)
	sort.SliceStable(out.DeviceRequests, func(i, j int) bool {
		a, b := out.DeviceRequests[i], out.DeviceRequests[j]
		if a.Driver != b.Driver {
			return a.Driver < b.Driver
		}
		return strconv.Itoa(a.Count) < strconv.Itoa(b.Count)
	})
	for i := range out.DeviceRequests {
		out.DeviceRequests[i].DeviceIDs = sortedCopy(out.DeviceRequests[i].DeviceIDs)
	}

	// Sysctls is a map; json.Marshal sorts map keys, so it needs no work here.
	return out
}

func sortedCopy(values []string) []string {
	if values == nil {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
