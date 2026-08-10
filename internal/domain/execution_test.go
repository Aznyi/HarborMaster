package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Container recreation domain tests.
//
// This is the model behind HarborMaster's largest privilege, so the tests are
// written around what must NOT be expressible:
//
//   - A cancellable state that has already changed the host.
//   - A verification that passes without every proof passing.
//   - A derived container name that is not a legal container name.
//   - A preservation comparison that passes on missing evidence.
//   - A recovery plan that claims nothing was changed when something may have
//     been.

const (
	execTestID    = "exec_0011223344556677889a"
	execOtherID   = "exec_ffeeddccbbaa99887766"
	execTestImage = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
)

// ------------------------------------------------------------ lifecycle --

// TestCancellableStatesNeverIncludeAMutatingOne is the most important assertion
// in this file.
//
// Cancelling after the mutation point would abandon a container that has been
// stopped and possibly replaced, leaving the host in a state nobody recorded.
// The two predicates must therefore be disjoint, and the pipeline relies on it.
func TestCancellableStatesNeverIncludeAMutatingOne(t *testing.T) {
	for _, state := range domain.ExecutionStates {
		if state.Cancellable() && state.Mutating() {
			t.Errorf("%s is both cancellable and mutating\n"+
				"\tcancelling a recreation that has already changed the host would leave it "+
				"in a state nobody recorded", state)
		}
	}
}

func TestExecutionStateClassification(t *testing.T) {
	cases := []struct {
		state       domain.ExecutionState
		active      bool
		cancellable bool
		mutating    bool
	}{
		{domain.ExecutionQueued, true, true, false},
		{domain.ExecutionValidating, true, true, false},
		{domain.ExecutionCapturing, true, true, false},
		{domain.ExecutionCreating, true, false, true},
		{domain.ExecutionStarting, true, false, true},
		{domain.ExecutionVerifying, true, false, true},
		{domain.ExecutionSucceeded, false, false, false},
		{domain.ExecutionFailed, false, false, false},
		{domain.ExecutionCancelled, false, false, false},
		{domain.ExecutionExpired, false, false, false},
	}

	if len(cases) != len(domain.ExecutionStates) {
		t.Fatalf("this table covers %d states, the domain has %d; a new state needs a row here",
			len(cases), len(domain.ExecutionStates))
	}

	for _, tc := range cases {
		if got := tc.state.Active(); got != tc.active {
			t.Errorf("%s.Active() = %v, want %v", tc.state, got, tc.active)
		}
		if got := tc.state.Terminal(); got != !tc.active {
			t.Errorf("%s.Terminal() = %v, want %v", tc.state, got, !tc.active)
		}
		if got := tc.state.Cancellable(); got != tc.cancellable {
			t.Errorf("%s.Cancellable() = %v, want %v", tc.state, got, tc.cancellable)
		}
		if got := tc.state.Mutating(); got != tc.mutating {
			t.Errorf("%s.Mutating() = %v, want %v", tc.state, got, tc.mutating)
		}
	}
}

func TestExecutionVocabulariesRejectUnknownValues(t *testing.T) {
	if domain.ValidExecutionState("running") {
		t.Error("ValidExecutionState accepted a state that is not in the vocabulary")
	}
	if domain.ValidExecutionState("") {
		t.Error("ValidExecutionState accepted the empty string; a stored state is never empty")
	}
	if domain.ValidExecutionFailure("catastrophe") {
		t.Error("ValidExecutionFailure accepted an unknown classification")
	}
	if !domain.ValidExecutionFailure("") {
		t.Error("ValidExecutionFailure rejected the empty string; a success has no failure")
	}
	if domain.ValidExecutionRefusal("becauseISaidSo") {
		t.Error("ValidExecutionRefusal accepted an unknown refusal")
	}
	if domain.ValidExecutionCheckpoint("halfway") {
		t.Error("ValidExecutionCheckpoint accepted an unknown checkpoint")
	}
}

// TestEveryFailureAndRefusalExplainsItself catches a value added to a
// vocabulary without the sentence an operator reads.
//
// A generic fallback would be worse than a compile error here: the message is
// the only thing telling someone what happened to their container.
func TestEveryFailureAndRefusalExplainsItself(t *testing.T) {
	fallbackFailure := domain.ExecutionFailure("not-a-real-failure").Explain()
	for _, failure := range domain.ExecutionFailures {
		if failure.Explain() == fallbackFailure {
			t.Errorf("failure %q falls through to the generic explanation", failure)
		}
	}

	fallbackRefusal := domain.ExecutionRefusal("not-a-real-refusal").Explain()
	for _, refusal := range domain.ExecutionRefusals {
		if refusal.Explain() == fallbackRefusal {
			t.Errorf("refusal %q falls through to the generic explanation", refusal)
		}
	}

	fallbackCheckpoint := domain.ExecutionCheckpoint("not-a-real-checkpoint").Explain()
	for _, checkpoint := range domain.ExecutionCheckpoints {
		if checkpoint.Explain() == fallbackCheckpoint {
			t.Errorf("checkpoint %q falls through to the generic explanation", checkpoint)
		}
	}
}

// TestFailuresBeforeTheMutationPointNeedNoOperator pins which classifications
// mean "nothing on the host to settle".
func TestFailuresBeforeTheMutationPointNeedNoOperator(t *testing.T) {
	harmless := map[domain.ExecutionFailure]bool{
		domain.ExecutionFailureNone:              true,
		domain.ExecutionFailurePreflight:         true,
		domain.ExecutionFailureCapture:           true,
		domain.ExecutionFailureSecretUnavailable: true,
	}

	for _, failure := range append(domain.ExecutionFailures, domain.ExecutionFailureNone) {
		want := !harmless[failure]
		if got := failure.NeedsOperator(); got != want {
			t.Errorf("%q.NeedsOperator() = %v, want %v", failure, got, want)
		}
	}
}

// TestVerificationPassesOnlyWhenEveryProofPasses is the fail-closed rule.
//
// The parked original is removed on the strength of this method, so an unknown
// must never count as a pass.
func TestVerificationPassesOnlyWhenEveryProofPasses(t *testing.T) {
	all := func() domain.ExecutionVerification {
		return domain.ExecutionVerification{
			Health:       domain.VerificationPassed,
			Image:        domain.VerificationPassed,
			Preservation: domain.VerificationPassed,
			Network:      domain.VerificationPassed,
		}
	}

	if !all().Passed() {
		t.Fatal("four passes must count as passed")
	}

	fields := []struct {
		name string
		set  func(*domain.ExecutionVerification, domain.VerificationResult)
	}{
		{"health", func(v *domain.ExecutionVerification, r domain.VerificationResult) { v.Health = r }},
		{"image", func(v *domain.ExecutionVerification, r domain.VerificationResult) { v.Image = r }},
		{"preservation", func(v *domain.ExecutionVerification, r domain.VerificationResult) { v.Preservation = r }},
		{"network", func(v *domain.ExecutionVerification, r domain.VerificationResult) { v.Network = r }},
	}

	for _, field := range fields {
		for _, result := range []domain.VerificationResult{
			domain.VerificationUnknown, domain.VerificationFailed, "",
		} {
			verification := all()
			field.set(&verification, result)
			if verification.Passed() {
				t.Errorf("%s = %q still counted as passed; a proof that did not pass must "+
					"never be treated as one", field.name, result)
			}
		}
	}
}

// ----------------------------------------------------------------- names --

func TestExecutionIDShapeIsValidated(t *testing.T) {
	generated := domain.NewExecutionID()
	if !domain.ValidExecutionID(generated) {
		t.Fatalf("a generated id %q does not satisfy its own validator", generated)
	}
	if !strings.HasPrefix(generated, domain.ExecutionIDPrefix) {
		t.Errorf("generated id %q does not carry the prefix", generated)
	}

	for _, bad := range []string{
		"", "exec_", "exec_zzzz", "exec_0011223344556677889", // one short
		"exec_0011223344556677889ab", // one long
		"acq_0011223344556677889a",   // wrong prefix
		"EXEC_0011223344556677889A",  // upper case
		"exec_0011223344556677889A",  // upper-case hex
		"../exec_0011223344556677889a",
	} {
		if domain.ValidExecutionID(bad) {
			t.Errorf("ValidExecutionID accepted %q", bad)
		}
	}
}

// TestDerivedNamesAreAlwaysLegalContainerNames is what stops a rename failing
// against the daemon after the original has already been stopped.
func TestDerivedNamesAreAlwaysLegalContainerNames(t *testing.T) {
	names := []string{
		"web", "a", "my_app.1", "compose-project-svc-1",
		strings.Repeat("n", domain.MaxRecreatableNameBytes),
	}

	for _, name := range names {
		parked, ok := domain.ParkedContainerName(name, execTestID)
		if !ok {
			t.Fatalf("could not derive a parked name for %q", name)
		}
		if !domain.ValidContainerName(parked) {
			t.Errorf("parked name %q is not a legal container name", parked)
		}

		quarantine, ok := domain.QuarantineContainerName(name, execTestID)
		if !ok {
			t.Fatalf("could not derive a quarantine name for %q", name)
		}
		if !domain.ValidContainerName(quarantine) {
			t.Errorf("quarantine name %q is not a legal container name", quarantine)
		}
		if parked == quarantine {
			t.Errorf("the parked and quarantine names collide for %q", name)
		}
	}
}

// TestDerivedNamesDifferPerExecution is what stops a second attempt colliding
// with the leftovers of a first.
func TestDerivedNamesDifferPerExecution(t *testing.T) {
	first, _ := domain.ParkedContainerName("web", execTestID)
	second, _ := domain.ParkedContainerName("web", execOtherID)

	if first == second {
		t.Fatal("two executions derived the same parked name; a retry after a failure would " +
			"collide with the container the first attempt left behind")
	}
}

func TestNameDerivationRefusesWhatItCannotProduce(t *testing.T) {
	tooLong := strings.Repeat("n", domain.MaxRecreatableNameBytes+1)
	if _, ok := domain.ParkedContainerName(tooLong, execTestID); ok {
		t.Error("a name too long to park was accepted; the rename would fail after the " +
			"original was already stopped")
	}
	if domain.RecreatableContainerName(tooLong) {
		t.Error("RecreatableContainerName accepted a name that cannot be parked")
	}

	for _, bad := range []string{"", ".hidden", "-flag", "web/../etc", "web;rm -rf /", "web name"} {
		if domain.ValidContainerName(bad) {
			t.Errorf("ValidContainerName accepted %q", bad)
		}
		if _, ok := domain.ParkedContainerName(bad, execTestID); ok {
			t.Errorf("ParkedContainerName accepted %q", bad)
		}
	}

	if _, ok := domain.ParkedContainerName("web", "not-an-execution-id"); ok {
		t.Error("ParkedContainerName accepted a malformed execution id")
	}
}

func TestNormaliseContainerNameStripsTheEngineSlash(t *testing.T) {
	if got := domain.NormaliseContainerName("/web"); got != "web" {
		t.Errorf("NormaliseContainerName(\"/web\") = %q, want \"web\"", got)
	}
	if got := domain.NormaliseContainerName("  /web  "); got != "web" {
		t.Errorf("NormaliseContainerName did not trim surrounding space: %q", got)
	}
}

func TestHarborMasterDerivedNamesAreRecognisable(t *testing.T) {
	parked, _ := domain.ParkedContainerName("web", execTestID)
	if !domain.IsHarborMasterDerivedName(parked) {
		t.Errorf("%q was not recognised as HarborMaster-derived", parked)
	}
	if domain.IsHarborMasterDerivedName("web") {
		t.Error("an ordinary name was reported as HarborMaster-derived")
	}
	if domain.IsHarborMasterDerivedName("web.hm-old-notanid") {
		t.Error("a name with a malformed id suffix was reported as HarborMaster-derived")
	}
}

// ---------------------------------------------------------- preservation --

// detailFor builds a container detail with one sensitive variable.
func detailFor(id, name string) domain.ContainerDetail {
	return domain.ContainerDetail{
		Overview: domain.ContainerSummary{
			ID:      id,
			ShortID: domain.ShortenID(id),
			Name:    name,
			Image:   domain.ParseImageRef("nginx:1.27.0"),
			RestartPolicy: domain.RestartPolicy{
				Name: "unless-stopped",
			},
		},
		Process: domain.Process{
			Command:    []string{"nginx", "-g", "daemon off;"},
			User:       "nginx",
			WorkingDir: "/",
		},
		Environment: []domain.EnvVar{
			{Name: "PORT", Value: "8080", Sensitivity: domain.SensitivityNormal, RawValue: "8080"},
			{
				Name: "DB_PASSWORD", Value: domain.MaskedValue,
				Sensitivity: domain.SensitivitySensitive, RawValue: "hunter2",
			},
		},
		Labels: []domain.Label{{Key: "app", Value: "web", Source: domain.LabelSourceUser}},
		Mounts: []domain.Mount{
			{Type: domain.MountTypeVolume, Source: "/var/lib/docker/volumes/data/_data",
				Destination: "/data", VolumeName: "data"},
		},
		Networks: []domain.NetworkAttachment{
			{NetworkName: "bridge", Aliases: []string{"web"}},
		},
		Security: domain.Security{
			ReadonlyRootfs:  true,
			NoNewPrivileges: true,
			CapDrop:         []string{"ALL"},
		},
	}
}

// fixedDigester is a stand-in for the installation's keyed hasher.
func fixedDigester(value string) string { return "d:" + value }

// TestPreservationSummaryCarriesNoSecretValue is the disclosure test.
//
// The projection is stored in the database and returned by the API, so a raw
// value reaching it would be a leak in three places at once.
func TestPreservationSummaryCarriesNoSecretValue(t *testing.T) {
	detail := detailFor(strings.Repeat("a", 64), "web")
	summary := domain.BuildPreservationSummary(detail, func(value string) string {
		return "hmac:" + strings.Repeat("0", 16)
	})

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(encoded), "hunter2") {
		t.Fatal("the preservation projection carried the raw secret value")
	}

	// The NAME must survive, because a variable disappearing is a real
	// configuration change and the comparison has to see it.
	if !strings.Contains(string(encoded), "DB_PASSWORD") {
		t.Error("the projection dropped the variable name; a removed secret would go unnoticed")
	}
}

// TestPreservationDetectsEveryKindOfChange walks the field set and asserts that
// a change anywhere is caught.
func TestPreservationDetectsEveryKindOfChange(t *testing.T) {
	base := detailFor(strings.Repeat("a", 64), "web")
	expected := domain.BuildPreservationSummary(base, fixedDigester)

	changes := []struct {
		name   string
		mutate func(*domain.ContainerDetail)
	}{
		{"a dropped capability restriction", func(d *domain.ContainerDetail) {
			d.Security.CapDrop = nil
		}},
		{"a writable root filesystem", func(d *domain.ContainerDetail) {
			d.Security.ReadonlyRootfs = false
		}},
		{"no-new-privileges turned off", func(d *domain.ContainerDetail) {
			d.Security.NoNewPrivileges = false
		}},
		{"privileged turned on", func(d *domain.ContainerDetail) {
			d.Security.Privileged = true
		}},
		{"a changed command", func(d *domain.ContainerDetail) {
			d.Process.Command = []string{"sh"}
		}},
		{"a changed user", func(d *domain.ContainerDetail) {
			d.Process.User = "root"
		}},
		{"a removed mount", func(d *domain.ContainerDetail) {
			d.Mounts = nil
		}},
		{"a re-pointed mount", func(d *domain.ContainerDetail) {
			d.Mounts[0].VolumeName = "other"
		}},
		{"a lost network", func(d *domain.ContainerDetail) {
			d.Networks = nil
		}},
		{"a changed restart policy", func(d *domain.ContainerDetail) {
			d.Overview.RestartPolicy.Name = "no"
		}},
		{"a removed environment variable", func(d *domain.ContainerDetail) {
			d.Environment = d.Environment[:1]
		}},
		{"a changed SECRET value", func(d *domain.ContainerDetail) {
			d.Environment[1].RawValue = "a-different-password"
		}},
		{"a changed label", func(d *domain.ContainerDetail) {
			d.Labels[0].Value = "elsewhere"
		}},
		{"a new memory limit", func(d *domain.ContainerDetail) {
			d.Resources.MemoryBytes = 1 << 30
		}},
	}

	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			mutated := detailFor(strings.Repeat("a", 64), "web")
			change.mutate(&mutated)

			actual := domain.BuildPreservationSummary(mutated, fixedDigester)
			report := domain.ComparePreservation(expected, actual)

			if report.Status == domain.VerificationPassed {
				t.Fatalf("%s was not detected; the recreation would have been reported as "+
					"faithful", change.name)
			}
			if len(report.Differences) == 0 {
				t.Error("the report failed but named no difference, so an operator would not " +
					"know what changed")
			}
		})
	}
}

// TestPreservationPassesOnAFaithfulRecreation is the other half: the check must
// not fire on the things that legitimately differ.
func TestPreservationPassesOnAFaithfulRecreation(t *testing.T) {
	// Both containers carry a DAEMON-GENERATED hostname — each its own short id
	// — which is the case that would fail every recreation if the projection
	// compared it.
	original := detailFor(strings.Repeat("a", 64), "web")
	original.Process.Hostname = strings.Repeat("a", 64)[:12]

	// A replacement: a different container id and short id, its own generated
	// hostname, a fresh address, and the new image. All of these differ on
	// EVERY successful recreation, so any one of them being compared would make
	// the feature unusable.
	replacement := detailFor(strings.Repeat("b", 64), "web")
	replacement.Process.Hostname = strings.Repeat("b", 64)[:12]
	replacement.Overview.Image = domain.ParseImageRef("nginx:1.27.1")
	replacement.Networks[0].IPv4Address = "172.17.0.9"
	replacement.Networks[0].MACAddress = "02:42:ac:11:00:09"
	replacement.Networks[0].EndpointID = strings.Repeat("c", 64)

	expected := domain.BuildPreservationSummary(original, fixedDigester)
	actual := domain.BuildPreservationSummary(replacement, fixedDigester)
	report := domain.ComparePreservation(expected, actual)

	if report.Status != domain.VerificationPassed {
		t.Fatalf("a faithful recreation was reported as %s: %v\n"+
			"\tdifferences: %+v", report.Status, report.Reason, report.Differences)
	}
}

// TestPreservationIgnoresTheLineageLabelHarborMasterWritesItself is the
// regression test for the defect the first live automated update exposed.
//
// A recreation stamps domain.LineageLabel onto the replacement so lineage
// survives a lost database. The original it was captured from does not carry
// that label, so a projection that compared it reported a difference on EVERY
// tracked recreation -- verification failed, auto-rollback undid a replacement
// that was in fact correct, and no container could ever be updated. That is not
// a theoretical failure: it is exactly what the first live run did.
//
// The three cases below are the three the second update of a workload walks
// through, and the reason the label is filtered from BOTH sides rather than
// only from the replacement.
func TestPreservationIgnoresTheLineageLabelHarborMasterWritesItself(t *testing.T) {
	const tracked = "docker.io/library/nginx:1.27"

	withLineage := func(detail domain.ContainerDetail, value string) domain.ContainerDetail {
		labels := append([]domain.Label(nil), detail.Labels...)
		detail.Labels = append(labels, domain.Label{
			Key: domain.LineageLabel, Value: value, Source: domain.LabelSourceUser,
		})
		return detail
	}

	original := detailFor(strings.Repeat("a", 64), "web")
	replacement := detailFor(strings.Repeat("b", 64), "web")

	for _, testCase := range []struct {
		name              string
		before, after     domain.ContainerDetail
		describesTheWorld string
	}{
		{
			name: "the first update adds the label",
			// The original was created by an operator and carries no lineage
			// label; the replacement HarborMaster creates carries one.
			before: original, after: withLineage(replacement, tracked),
			describesTheWorld: "a workload HarborMaster is updating for the first time",
		},
		{
			name: "a later update carries it on both sides",
			// The original is itself a previous replacement.
			before: withLineage(original, tracked), after: withLineage(replacement, tracked),
			describesTheWorld: "the second and every subsequent update of the same workload",
		},
		{
			name: "the tracked reference moved between updates",
			// A series upgrade rewrites the tag the label records. HarborMaster
			// owns this value, so changing it is not a preservation failure.
			before:            withLineage(original, tracked),
			after:             withLineage(replacement, "docker.io/library/nginx:1.28"),
			describesTheWorld: "a minor upgrade that moved the tracking tag",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			report := domain.ComparePreservation(
				domain.BuildPreservationSummary(testCase.before, fixedDigester),
				domain.BuildPreservationSummary(testCase.after, fixedDigester),
			)
			if report.Status != domain.VerificationPassed {
				t.Fatalf("%s was reported as %s: %s\n\tdifferences: %+v\n"+
					"\ta label HarborMaster writes itself must not fail the operator's "+
					"configuration check", testCase.describesTheWorld,
					report.Status, report.Reason, report.Differences)
			}
		})
	}

	// The filter must be narrow. An OPERATOR's label going missing across a
	// recreation is still a preservation failure -- if this passed, the fix
	// above would have blinded the check it was meant to keep working.
	lost := withLineage(replacement, tracked)
	lost.Labels = lost.Labels[1:] // drop "app=web", keep the lineage label
	report := domain.ComparePreservation(
		domain.BuildPreservationSummary(withLineage(original, tracked), fixedDigester),
		domain.BuildPreservationSummary(lost, fixedDigester),
	)
	if report.Status == domain.VerificationPassed {
		t.Error("a replacement that lost one of the operator's labels passed verification; " +
			"the lineage filter is too wide")
	}
}

// TestPreservationIsUnverifiableAcrossKeys fails closed rather than comparing
// digests that cannot be compared.
func TestPreservationIsUnverifiableAcrossKeys(t *testing.T) {
	detail := detailFor(strings.Repeat("a", 64), "web")

	expected := domain.BuildPreservationSummary(detail, fixedDigester)
	expected.DigestKeyID = "key-one"
	actual := domain.BuildPreservationSummary(detail, fixedDigester)
	actual.DigestKeyID = "key-two"

	report := domain.ComparePreservation(expected, actual)
	if report.Status == domain.VerificationPassed {
		t.Fatal("summaries under different keys were compared as equal")
	}
	if !report.Unverifiable {
		t.Error("the report did not say it was unverifiable, so an operator would read a " +
			"key mismatch as a real configuration difference")
	}
}

// TestPreservationWithoutADigesterCannotPass is the fail-closed case for a
// deployment with no hasher wired.
func TestPreservationWithoutADigesterCannotPass(t *testing.T) {
	original := detailFor(strings.Repeat("a", 64), "web")
	changed := detailFor(strings.Repeat("a", 64), "web")
	changed.Environment[1].RawValue = "a-different-password"

	expected := domain.BuildPreservationSummary(original, nil)
	actual := domain.BuildPreservationSummary(changed, nil)

	// Without a digester the two sensitive values render identically, so this
	// specific difference is invisible -- which is exactly why the service
	// refuses to run without a hasher rather than relying on this comparison.
	report := domain.ComparePreservation(expected, actual)
	if report.Status == domain.VerificationPassed {
		for _, field := range expected.Fields {
			if field.Name != "environment" {
				continue
			}
			if !strings.Contains(field.Value, "unverifiable") {
				t.Fatal("a nil digester produced a comparable token for a secret; two " +
					"different passwords would compare equal with nothing saying so")
			}
		}
	}
}

func TestPreservationReportBoundsItsDifferences(t *testing.T) {
	expected := domain.PreservationSummary{Fingerprint: "a"}
	actual := domain.PreservationSummary{Fingerprint: "b"}

	for i := 0; i < domain.MaxPreservationDifferences+10; i++ {
		name := "field." + string(rune('a'+i%26)) + string(rune('a'+i/26))
		expected.Fields = append(expected.Fields,
			domain.PreservationField{Name: name, Value: "one"})
		actual.Fields = append(actual.Fields,
			domain.PreservationField{Name: name, Value: "two"})
	}

	report := domain.ComparePreservation(expected, actual)
	if len(report.Differences) > domain.MaxPreservationDifferences {
		t.Errorf("recorded %d differences, the bound is %d",
			len(report.Differences), domain.MaxPreservationDifferences)
	}
	if !report.Truncated {
		t.Error("the report was truncated without saying so")
	}
}

func TestPreservationOnEmptyProjectionsIsUnverifiable(t *testing.T) {
	report := domain.ComparePreservation(
		domain.PreservationSummary{}, domain.PreservationSummary{})

	if report.Status == domain.VerificationPassed {
		t.Fatal("two empty projections compared as equal; nothing was checked")
	}
	if !report.Unverifiable {
		t.Error("an empty comparison did not report itself unverifiable")
	}
}

// ------------------------------------------------------------- recovery --

// TestRecoveryPlanNeverClaimsNothingChangedWhenSomethingMightHave is the
// honesty test.
//
// A process killed between issuing a stop and recording it has an empty
// checkpoint, exactly like one that never started. Telling the second operator
// "nothing was changed" would be a confident, specific, false statement about a
// container that may well be down.
func TestRecoveryPlanNeverClaimsNothingChangedWhenSomethingMightHave(t *testing.T) {
	untouched := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:   execTestID,
		ContainerName: "web",
		OriginalID:    strings.Repeat("a", 64),
		Checkpoint:    domain.CheckpointNone,
	})
	if untouched.Urgency != domain.RecoveryInformational {
		t.Errorf("an untouched host produced urgency %q, want informational", untouched.Urgency)
	}
	if untouched.ServiceInterrupted {
		t.Error("an untouched host was reported as service-interrupted")
	}

	uncertain := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:       execTestID,
		ContainerName:     "web",
		OriginalID:        strings.Repeat("a", 64),
		Checkpoint:        domain.CheckpointNone,
		MutationAttempted: true,
	})
	if uncertain.Urgency != domain.RecoveryUrgent {
		t.Errorf("an unconfirmed stop produced urgency %q, want urgent", uncertain.Urgency)
	}
	if !uncertain.ServiceInterrupted {
		t.Error("an unconfirmed stop was not reported as possibly service-interrupted")
	}
	if strings.Contains(uncertain.Situation, "Nothing on this host was changed") {
		t.Fatal("an unconfirmed stop claimed nothing was changed")
	}
	if len(uncertain.Steps) == 0 {
		t.Fatal("an unconfirmed stop produced no steps to follow")
	}
}

// TestRecoveryPlansCoverEveryCheckpoint catches a checkpoint added without a
// plan for the situation it describes.
func TestRecoveryPlansCoverEveryCheckpoint(t *testing.T) {
	for _, checkpoint := range domain.ExecutionCheckpoints {
		plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
			ExecutionID:    execTestID,
			ContainerName:  "web",
			OriginalID:     strings.Repeat("a", 64),
			ParkedName:     "web.hm-old-" + execTestID,
			ReplacementID:  strings.Repeat("b", 64),
			QuarantineName: "web.hm-failed-" + execTestID,
			Checkpoint:     checkpoint,
		})

		if plan == nil {
			t.Fatalf("checkpoint %q produced no plan", checkpoint)
		}
		if plan.Situation == "" {
			t.Errorf("checkpoint %q produced a plan with no statement of the situation", checkpoint)
		}
		if strings.Contains(plan.Situation, "not certain what state") {
			t.Errorf("checkpoint %q fell through to the unrecognised-checkpoint plan; it needs "+
				"its own case", checkpoint)
		}
		if len(plan.Steps) > domain.MaxRecoverySteps {
			t.Errorf("checkpoint %q produced %d steps, the bound is %d",
				checkpoint, len(plan.Steps), domain.MaxRecoverySteps)
		}
		for i, step := range plan.Steps {
			if step.Order != i+1 {
				t.Errorf("checkpoint %q: step %d carries order %d", checkpoint, i, step.Order)
			}
			if step.Description == "" {
				t.Errorf("checkpoint %q: step %d has no description", checkpoint, i)
			}
		}
	}
}

// TestRecoveryPlanFlagsAnInterruptedService pins which checkpoints mean the
// container is down.
func TestRecoveryPlanFlagsAnInterruptedService(t *testing.T) {
	down := map[domain.ExecutionCheckpoint]bool{
		domain.CheckpointOriginalStopped:        true,
		domain.CheckpointOriginalParked:         true,
		domain.CheckpointReplacementCreated:     true,
		domain.CheckpointReplacementStarted:     true,
		domain.CheckpointReplacementQuarantined: true,
	}

	for _, checkpoint := range domain.ExecutionCheckpoints {
		plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
			ExecutionID:   execTestID,
			ContainerName: "web",
			OriginalID:    strings.Repeat("a", 64),
			ParkedName:    "web.hm-old-" + execTestID,
			Checkpoint:    checkpoint,
		})
		if got := plan.ServiceInterrupted; got != down[checkpoint] {
			t.Errorf("checkpoint %q: serviceInterrupted = %v, want %v",
				checkpoint, got, down[checkpoint])
		}
	}
}

// TestRecoveryPlanNeverRecommendsRemovingTheOriginalWhileTheReplacementIsUnproved
// is the "no rollback, and no premature cleanup either" test.
func TestRecoveryPlanNeverRecommendsRemovingTheOriginalWhileTheReplacementIsUnproved(t *testing.T) {
	parked := "web.hm-old-" + execTestID

	for _, checkpoint := range []domain.ExecutionCheckpoint{
		domain.CheckpointOriginalStopped,
		domain.CheckpointOriginalParked,
		domain.CheckpointReplacementCreated,
		domain.CheckpointReplacementStarted,
		domain.CheckpointReplacementQuarantined,
	} {
		plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
			ExecutionID:    execTestID,
			ContainerName:  "web",
			OriginalID:     strings.Repeat("a", 64),
			ParkedName:     parked,
			ReplacementID:  strings.Repeat("b", 64),
			QuarantineName: "web.hm-failed-" + execTestID,
			Checkpoint:     checkpoint,
		})

		for _, step := range plan.Steps {
			if strings.Contains(step.Command, "docker rm "+parked) {
				t.Errorf("checkpoint %q recommends removing the parked original, which is the "+
					"only way back to a working container", checkpoint)
			}
		}
	}
}

func TestRecoveryStepsMarkDestructiveCommands(t *testing.T) {
	plan := domain.BuildRecoveryPlan(domain.RecoveryContext{
		ExecutionID:    execTestID,
		ContainerName:  "web",
		OriginalID:     strings.Repeat("a", 64),
		ParkedName:     "web.hm-old-" + execTestID,
		ReplacementID:  strings.Repeat("b", 64),
		QuarantineName: "web.hm-failed-" + execTestID,
		Checkpoint:     domain.CheckpointReplacementQuarantined,
	})

	for _, step := range plan.Steps {
		removes := strings.Contains(step.Command, "docker rm ")
		if removes && !step.Destructive {
			t.Errorf("step %d removes something but is not marked destructive: %q",
				step.Order, step.Command)
		}
		if !removes && step.Destructive {
			t.Errorf("step %d is marked destructive but removes nothing: %q",
				step.Order, step.Command)
		}
	}
}

// ------------------------------------------------------------- the target --

func TestExecutionTargetRequiresADigest(t *testing.T) {
	valid := domain.ExecutionTarget{
		Registry: "docker.io", Repository: "library/nginx", Digest: execTestImage,
	}
	if !valid.Valid() {
		t.Fatal("a well-formed target was rejected")
	}
	if got := valid.PinnedReference(); got != "docker.io/library/nginx@"+execTestImage {
		t.Errorf("PinnedReference() = %q", got)
	}

	for _, bad := range []domain.ExecutionTarget{
		{Registry: "docker.io", Repository: "library/nginx"},
		{Registry: "docker.io", Repository: "library/nginx", Digest: "latest"},
		{Registry: "docker.io", Digest: execTestImage},
		{Registry: "127.0.0.1:5000", Repository: "x", Digest: execTestImage},
	} {
		if bad.Valid() {
			t.Errorf("an unpinned or unreachable target was accepted: %+v", bad)
		}
	}
}

// TestExecutionJSONCarriesNoInternalRowID keeps the API contract stable.
func TestExecutionJSONCarriesNoInternalRowID(t *testing.T) {
	encoded, err := json.Marshal(domain.Execution{ID: 42, ExecutionID: execTestID})
	if err != nil {
		t.Fatalf("marshal execution: %v", err)
	}
	if strings.Contains(string(encoded), "42") {
		t.Error("the internal row id reached the API contract")
	}
}
