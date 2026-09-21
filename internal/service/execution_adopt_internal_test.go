package service

import (
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The adoption decision, in isolation.
//
// A container holding the production name after a failed create may be the
// replacement that create produced after the client gave up on it, or it may be
// anything else. HarborMaster adopts it only on evidence it wrote itself and can
// re-verify: its own ownership labels naming this execution and this original,
// the production name, and the approved image. Anything short of that refuses,
// and a refusal touches nothing.

const (
	adoptExecutionID = "exec_00112233445566778899"
	adoptOriginalID  = "1111111111111111111111111111111111111111111111111111111111111111"
	adoptCandidateID = "2222222222222222222222222222222222222222222222222222222222222222"
	adoptDigest      = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	adoptImageID     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func adoptExecution() domain.Execution {
	return domain.Execution{
		ExecutionID:   adoptExecutionID,
		ContainerID:   adoptOriginalID,
		ContainerName: "web",
		Checkpoint:    domain.CheckpointOriginalParked,
		Target: domain.ExecutionTarget{
			Registry: "docker.io", Repository: "library/nginx",
			Digest: adoptDigest, ImageID: adoptImageID,
		},
	}
}

func adoptCandidate() domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:      adoptCandidateID,
			Name:    "/web",
			Image:   domain.ParseImageRef("docker.io/library/nginx@" + adoptDigest),
			ImageID: adoptImageID,
		},
		Labels: []domain.Label{
			{Key: domain.LabelExecutionOwner, Value: adoptExecutionID},
			{Key: domain.LabelReplacementOf, Value: adoptOriginalID},
		},
	}
}

func TestAnExactOwnershipMatchIsAdopted(t *testing.T) {
	if reason := adoptionRefusal(adoptExecution(), adoptCandidate()); reason != "" {
		t.Fatalf("an exact match was refused: %s", reason)
	}
}

func TestEveryShortfallInOwnershipEvidenceRefusesAdoption(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.Execution, *domain.ContainerDetail)
		want   string
	}{
		{"wrong execution label", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Labels[0].Value = "exec_ffffffffffffffffffff"
		}, "execution"},
		{"missing execution label", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Labels = c.Labels[1:]
		}, "execution"},
		{"no labels at all", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Labels = nil
		}, "execution"},
		{"conflicting original lineage", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Labels[1].Value = "3333333333333333333333333333333333333333333333333333333333333333"
		}, "original"},
		{"missing original lineage", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Labels = c.Labels[:1]
		}, "original"},
		{"right labels, wrong image", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Overview.Image = domain.ParseImageRef("docker.io/library/nginx:latest")
			c.Overview.ImageID = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}, "image"},
		{"right labels, wrong name", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Overview.Name = "/web-copy"
		}, "name"},
		{"the original itself", func(e *domain.Execution, c *domain.ContainerDetail) {
			c.Overview.ID = e.ContainerID
		}, "original"},
		{"not a full id", func(_ *domain.Execution, c *domain.ContainerDetail) {
			c.Overview.ID = "abc123"
		}, "id"},
		{"execution never parked", func(e *domain.Execution, _ *domain.ContainerDetail) {
			e.Checkpoint = domain.CheckpointOriginalStopped
		}, "checkpoint"},
		{"replacement already recorded", func(e *domain.Execution, _ *domain.ContainerDetail) {
			e.ReplacementID = adoptCandidateID
		}, "recorded"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			execution := adoptExecution()
			candidate := adoptCandidate()
			testCase.mutate(&execution, &candidate)

			reason := adoptionRefusal(execution, candidate)
			if reason == "" {
				t.Fatal("the candidate was adopted without sufficient evidence")
			}
			if !strings.Contains(reason, testCase.want) {
				t.Errorf("refusal %q does not name %q", reason, testCase.want)
			}
		})
	}
}
