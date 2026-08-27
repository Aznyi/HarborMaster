package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/config"
	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Stage 17.8b onboarding: structural proof that a page which EXPLAINS cannot
// become a page which ACTS.
//
// # The failure this prevents
//
// Onboarding exists because a fresh installation looks idle, and the tempting
// fix for "nothing is happening" is to make the page make something happen:
// generate a plan so readiness has data, create a policy so the count is not
// zero, write an environment variable so the engine comes on. Each would turn
// opening a page into changing the host.
//
// The behavioural half is asserted in the frontend tests, which open the page
// and assert every request was a read. This is the structural half: the source
// may not even name the paths that would do it.

// onboardingSources are the files that render or compose first-run state.
var onboardingSources = []string{
	filepath.Join("web", "src", "api", "firstRun.ts"),
	filepath.Join("web", "src", "components", "AutomationOnboarding.tsx"),
}

func onboardingSource(t *testing.T, rel string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(moduleRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(source)
}

// TestOnboardingCannotWriteAnything is the load-bearing guard.
func TestOnboardingCannotWriteAnything(t *testing.T) {
	forbidden := map[string]string{
		"generatePlans":       "onboarding must not start a planning pass",
		"/plans/generate":     "onboarding must not start a planning pass",
		"createUpdatePolicy":  "onboarding invites a policy; it does not write one",
		"updateUpdatePolicy":  "onboarding does not edit a policy",
		"archiveUpdatePolicy": "onboarding does not withdraw a policy",
		"resumeAutomation":    "resuming a pause is Stage 17.6's deliberate act",
		"approvePlan":         "approving a plan is Stage 17.7's deliberate act",
		"revokePlanApproval":  "onboarding does not withdraw an approval",
		"runAutomationPass":   "onboarding does not run a pass",
		"requestAcquisition":  "onboarding downloads nothing",
		"requestExecution":    "onboarding changes no container",
		`method: "POST"`:      "onboarding reads; it does not post",
		`"PATCH"`:             "onboarding reads",
		`"DELETE"`:            "onboarding reads",
	}

	for _, rel := range onboardingSources {
		source := onboardingSource(t, rel)
		for symbol, why := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("%s names %s\n\t%s", rel, symbol, why)
			}
		}
	}
}

// TestOnboardingCannotWriteConfiguration pins §7.
//
// The capability settings are startup environment variables. A page offering to
// write them would be offering to edit a file it does not own and restart a
// process it is running inside -- and an operator who believed the button had
// worked would be worse off than one who was told to do it themselves.
func TestOnboardingCannotWriteConfiguration(t *testing.T) {
	for _, rel := range onboardingSources {
		source := onboardingSource(t, rel)
		for _, symbol := range []string{
			"writeFile", "saveConfig", "updateConfig", "/settings/save",
			"restartHarborMaster", "reloadConfig", ".env",
		} {
			if strings.Contains(source, symbol) {
				t.Errorf("%s names %s\n"+
					"\tonboarding shows the settings and never writes them", rel, symbol)
			}
		}
	}
}

// TestTheFirstRunProjectionDecidesNothingAboutEligibility is §1's boundary.
//
// The client may choose which onboarding sentence to show. It may not work out
// whether a container can be updated: that is the backend's, and a second
// implementation would drift from it exactly as the Stage 17.4 preview drifted
// from the pass before it was made to share one.
func TestTheFirstRunProjectionDecidesNothingAboutEligibility(t *testing.T) {
	source := onboardingSource(t, filepath.Join("web", "src", "api", "firstRun.ts"))

	forbidden := map[string]string{
		"recommendation":     "the recommendation is the planner's",
		"proceedWithCaution": "the recommendation floor is the policy's",
		// Quoted, deliberately: `manualReviews` is a COUNT the server supplies
		// and the panel repeats. What is forbidden is comparing against the
		// recommendation VALUE, which would be the client deciding.
		`"manualReview"`:    "manual review is the planner's recommendation",
		"updateType":        "the size of a change is the planner's",
		"riskScore":         "risk is the planner's",
		"snapshotAvailable": "snapshot evidence is the execution preflight's",
		"dependencyState":   "dependency ordering is Phase 16's",
		"selfUpdate":        "the self-update refusal is DecideAutomation's step 0",
		"Mutates":           "whether a mode acts is the server's",
	}
	for symbol, why := range forbidden {
		if strings.Contains(source, symbol) {
			t.Errorf("firstRun.ts names %s\n"+
				"\t%s; the client chooses a sentence, it does not decide eligibility",
				symbol, why)
		}
	}

	// And it must still be a projection over server-supplied facts.
	if !strings.Contains(source, "export function describeFirstRun") {
		t.Fatal("the projection is gone; this guard checks nothing")
	}
}

// TestNoSecondReadinessOrOnboardingRouteWasAdded is §28's stopping condition,
// asserted rather than asserted-in-prose.
//
// The whole architectural decision for this stage was to compose existing reads
// in the client rather than add a backend endpoint whose only job is to
// concatenate them. This fails if one appeared anyway.
func TestNoSecondReadinessOrOnboardingRouteWasAdded(t *testing.T) {
	routes, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "api", "routes.go"))
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	source := string(routes)

	for _, path := range []string{
		"/onboarding", "/first-run", "/firstrun", "/setup", "/engine-state",
	} {
		if strings.Contains(source, `"`+path) {
			t.Errorf("the route table names %s\n"+
				"\tStage 17.8b composes existing reads in the client; a backend "+
				"endpoint that concatenates them was the rejected alternative", path)
		}
	}

	// Exactly one readiness route, still.
	if got := strings.Count(source, "/automation/readiness"); got != 2 {
		t.Errorf("found %d readiness route entries, want 2 (the method and its "+
			"bare 405 partner); a second readiness surface is a second model", got)
	}
}

// TestThePlannerStartupWiringIsUnchanged pins the Stage 17.8 audit finding.
//
// The planner already runs at startup and is already signalled by inventory
// refresh. That is why no trigger was added. If the wiring goes, the
// assessment-pending state stops resolving on its own and the onboarding copy
// ("no action is required") becomes false.
func TestThePlannerStartupWiringIsUnchanged(t *testing.T) {
	main, err := os.ReadFile(filepath.Join(moduleRoot(t), "cmd", "harbormaster", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(main), "inventory.AddRefreshObserver(planner)") {
		t.Error("the planner is no longer signalled by an inventory refresh\n" +
			"\tthe onboarding copy promises the assessment resolves without " +
			"operator action, which depends on this wiring")
	}

	worker, err := os.ReadFile(filepath.Join(
		moduleRoot(t), "internal", "service", "planner_worker.go"))
	if err != nil {
		t.Fatalf("read planner_worker.go: %v", err)
	}
	source := string(worker)
	if !strings.Contains(source, "func (s *PlannerService) InventoryRefreshed") {
		t.Error("the planner no longer observes inventory refreshes")
	}
	if !strings.Contains(source, "if s.cfg.GenerateOnStartup {") {
		t.Error("the planner no longer runs a pass at startup")
	}
	// And there is still exactly one loop driving it.
	if got := strings.Count(source, "func (s *PlannerService) Run("); got != 1 {
		t.Errorf("found %d planner run loops, want 1; a second is a second planner", got)
	}
}

// TestTheAdvertisedCapabilitiesAreOnesTheProcessWillStartWith closes the loop
// between the onboarding list and the rules that actually gate startup.
//
// # The defect this exists for
//
// The capability list is printed to an operator as environment variables to
// apply. If that set is one config validation refuses, applying it exactly and
// recreating the container leaves HarborMaster unable to boot -- onboarding
// instructions that take the installation down.
//
// This asserts the two agree by running the real validator rather than by
// reading either one's source.
func TestTheAdvertisedCapabilitiesAreOnesTheProcessWillStartWith(t *testing.T) {
	capabilityRules := []string{
		"ACQUISITION_ENABLED", "EXECUTION_ENABLED",
		"ROLLBACK_ENABLED", "AUTOMATION_ENABLED",
	}

	// A config carrying exactly what onboarding advertises.
	required := domain.RequiredForAutomation()
	var cfg config.Config
	cfg.Acquisition.Enabled = required.Acquisition
	cfg.Execution.Enabled = required.Execution
	cfg.Rollback.Enabled = required.Rollback
	cfg.Automation.Enabled = required.Automation

	// Validate reports every problem with a zero Config, most of them about
	// ports and paths this test says nothing about. Only the capability
	// combination rules are in question.
	if err := cfg.Validate(); err != nil {
		for _, rule := range capabilityRules {
			if strings.Contains(err.Error(), rule+" requires") {
				t.Errorf("the advertised capability set is refused at startup: %s requires ...\n"+
					"\tonboarding prints these variables for an operator to apply; a set "+
					"the process will not start with is instructions that stop HarborMaster "+
					"from booting\n\tfull error: %v", rule, err)
			}
		}
	}

	// Non-vacuity: the validator really does police this, and dropping one
	// capability from the advertised set really is caught.
	cfg.Rollback.Enabled = false
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "AUTOMATION_ENABLED requires") {
		t.Fatal("dropping rollback was not refused by config validation; this test " +
			"would pass whatever RequiredForAutomation returned")
	}
}

// TestTheClientFallbackCapabilitySetMatchesGo closes the hole live acceptance
// found in Stage 17.9.
//
// # The defect this exists for
//
// RequiredForAutomation was corrected in Stage 17.8b to always include
// rollback, because the process refuses to start without it. But the onboarding
// component carried its OWN copy of the set, used whenever the server names
// nothing -- which is exactly the fresh installation the panel exists for, since
// nothing is required to do nothing.
//
// So a first-run operator was shown:
//
//	HARBORMASTER_ACQUISITION_ENABLED=true
//	HARBORMASTER_EXECUTION_ENABLED=true
//	HARBORMASTER_AUTOMATION_ENABLED=true
//
// Applying exactly that and recreating the container leaves HarborMaster
// refusing to boot. The Go fix was real but incomplete: correcting a rule that
// exists in two places only fixes the copy you edited.
func TestTheClientFallbackCapabilitySetMatchesGo(t *testing.T) {
	source := onboardingSource(t, filepath.Join("web", "src", "api", "firstRun.ts"))

	start := strings.Index(source, "export const REQUIRED_FOR_AUTOMATION")
	if start < 0 {
		t.Fatal("REQUIRED_FOR_AUTOMATION is gone from firstRun.ts; the onboarding " +
			"panel has no single fallback set and this guard checks nothing")
	}
	block := source[start:]
	if end := strings.Index(block, "];"); end > 0 {
		block = block[:end]
	}

	required := domain.RequiredForAutomation()
	want := map[string]bool{
		"acquisition": required.Acquisition,
		"execution":   required.Execution,
		"automation":  required.Automation,
		"rollback":    required.Rollback,
	}
	for name, needed := range want {
		listed := strings.Contains(block, `"`+name+`"`)
		if needed && !listed {
			t.Errorf("the client fallback omits %q, which Go requires\n"+
				"\tthe panel prints this set as environment variables to apply; a set "+
				"the process will not start with is instructions that stop HarborMaster "+
				"from booting", name)
		}
		if !needed && listed {
			t.Errorf("the client fallback names %q, which Go does not require\n"+
				"\ttelling an operator to enable a capability nothing needs widens what "+
				"HarborMaster may do for nothing", name)
		}
	}

	// Non-vacuity: the set really is non-empty, so a guard that matched an empty
	// block would not pass by accident.
	if len(want) != 4 {
		t.Fatalf("expected four capabilities to check, got %d", len(want))
	}
}

// TestBackgroundServicesAreCountedAsTheyStart pins the Stage 17.9 shutdown
// panic shut.
//
// # What live acceptance found
//
// The composition root declared `background.Add(13)` above a hand-written list
// of goroutines. The list had grown to fourteen. Every shutdown ended:
//
//	panic: sync: negative WaitGroup counter
//	main.run.func5() cmd/harbormaster/main.go:995
//
// The panic is the loud half. The quiet half is worse: Wait() returns when the
// counter reaches zero, so the bounded wait could return while a service was
// still running, and the deferred db.Close() would then close the database
// underneath a final event flush -- the precise ordering the shutdown comment
// exists to guarantee.
//
// A count written by hand next to a list is a count that will disagree with the
// list. This forbids the shape rather than checking the number.
func TestBackgroundServicesAreCountedAsTheyStart(t *testing.T) {
	main, err := os.ReadFile(filepath.Join(moduleRoot(t), "cmd", "harbormaster", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	source := string(main)

	// A bulk Add is a hand-maintained count. Only Add(1), from the helper that
	// starts exactly one goroutine, is allowed.
	//
	// Comments are stripped first: the fix documents the old shape by quoting
	// it, and a guard that fired on its own explanation would be unfixable.
	var code strings.Builder
	for _, line := range strings.Split(source, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}

	bulk := regexp.MustCompile(`background\.Add\((\d+)\)`)
	for _, match := range bulk.FindAllStringSubmatch(code.String(), -1) {
		if match[1] != "1" {
			t.Errorf("main.go declares %s\n"+
				"\ta background-service count written by hand drifts from the list "+
				"it sits above; add as each goroutine starts instead", match[0])
		}
	}

	// And the helper must still be the thing that starts them, with its Add and
	// its Done paired in one place.
	if !strings.Contains(source, "start := func(run func(context.Context)) {") {
		t.Fatal("the start helper is gone; background services are being " +
			"accounted for some other way and this guard checks nothing")
	}
	if got := strings.Count(source, "background.Done()"); got != 1 {
		t.Errorf("found %d background.Done() calls, want exactly 1 (the helper's)", got)
	}

	// Non-vacuity: there really are services being started this way.
	if got := strings.Count(source, "\tstart("); got < 10 {
		t.Errorf("found %d started background services; the composition root is "+
			"not where this guard thinks it is", got)
	}
}
