package service_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/docker"
	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// What must NOT produce a notification.
//
// Deciding an event is not worth a message is a product decision, and an
// undocumented one erodes: somebody adds a message because it seemed helpful,
// the channel gets noisier, and the operator who muted it misses the failed
// rollback. These tests are where those decisions are written down in a form
// that fails the build when one is reversed by accident.

// ------------------------------------------------------------- cleanup --

// TestImageCleanupCannotNotify pins the C4A decision.
//
// Routine image removal is MAINTENANCE. It changes nothing an operator is
// waiting on, it happens on a twelve-hour timer, and on an estate with any
// history it would be the single chattiest thing HarborMaster does. Its record
// is the security audit log, where `image.removed` is written for every removal
// that actually happened -- a durable, queryable account rather than a message
// that scrolls past.
//
// Structural rather than behavioural: the service has nowhere to put a notifier,
// so this cannot be reversed by adding a call. It has to be reversed by adding
// a field, which is a change somebody has to justify.
func TestImageCleanupCannotNotify(t *testing.T) {
	t.Parallel()

	options := reflect.TypeOf(service.ImageCleanupOptions{})
	notifier := reflect.TypeOf((*service.Notifier)(nil)).Elem()

	for i := 0; i < options.NumField(); i++ {
		field := options.Field(i)
		if field.Type == notifier || field.Type.Implements(notifier) {
			t.Fatalf("ImageCleanupOptions.%s can carry a notifier.\n\n"+
				"Routine cleanup is maintenance, not lifecycle news. Its record "+
				"is the audit log. A message for every removed image would be "+
				"the noisiest thing HarborMaster sends and would train an "+
				"operator to mute the channel that also carries failed "+
				"rollbacks.", field.Name)
		}
	}
}

func TestACleanupPassRaisesNothingAndChangesNoUpdateState(t *testing.T) {
	t.Parallel()

	// The behavioural half. A pass that removes an image must not produce
	// anything an operator could mistake for an update outcome -- and a pass
	// that FAILS to remove one must not either, because a maintenance problem
	// is not a container problem.
	settled := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return settled.Add(365 * 24 * time.Hour) }

	for _, testCase := range []struct {
		name    string
		breakIt func(*docker.FakeImagePruner)
	}{
		{"a removal that succeeds", func(*docker.FakeImagePruner) {}},
		{"a removal the daemon refuses", func(p *docker.FakeImagePruner) {
			p.MarkInUse(cleanupOld)
		}},
		{"a removal that errors outright", func(p *docker.FakeImagePruner) {
			p.Err = docker.ErrUnreachable
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pruner := docker.NewFakeImagePruner()
			pruner.Add(cleanupOld)
			testCase.breakIt(pruner)

			cleanup := service.NewImageCleanupService(service.ImageCleanupOptions{
				Store: &fakeRetentionStore{
					candidates: []store.ImageCleanupCandidate{{
						ImageID:          cleanupOld,
						ContainerName:    "web",
						SettledAt:        settled,
						NewerGenerations: 3,
					}},
					references: map[string]store.ImageReferences{},
				},
				Runtime: docker.NewFake(),
				Pruner:  pruner,
				Config:  cleanupConfig(),
				Now:     clock,
			})

			// The pass completes and reports its own counts. Nothing about it
			// reaches the update lifecycle: there is no execution to fail, no
			// rollback to start, and no notifier to tell.
			pass := cleanup.RunPass(context.Background())
			if pass.RanAt.IsZero() {
				t.Fatal("the pass did not run")
			}
			if pass.Considered != 1 {
				t.Errorf("considered = %d, want 1", pass.Considered)
			}
		})
	}
}

// ------------------------------------------------------- monitor only --

// TestMonitorOnlyIsToldAboutUpdatesExactlyOncePerPlan documents the decision.
//
// # The decision
//
// Monitor-only KEEPS its notification. HarborMaster already treats
// `update.discovered` as the way a deployment that does not let anything act
// still learns an update exists -- the planner raises it, not automation, so it
// works with automation switched off, which is the default. C4B does not invent
// a new default and does not take that away.
//
// # Why it is not noisy
//
// It is raised only for a plan the planner classified as NEW. A plan whose
// fingerprint matches the stored one never reaches the raise, so a planner
// running hourly says a thing once rather than twenty-four times. The dedup key
// names the container and the version it was told about, so a rule with a
// cooldown deduplicates the same news and still reports the NEXT version.
//
// # And it mutates nothing
//
// Discovery is the planner, which holds no Docker mutation capability at all --
// pinned by the architecture tests, not by this one. What this pins is that the
// notification carries no instruction and no identifier anything could act on.
func TestMonitorOnlyIsToldAboutUpdatesExactlyOncePerPlan(t *testing.T) {
	t.Parallel()

	notifier := &recordingNotifier{}
	service.NotifyUpdateDiscovered(notifier, "web",
		"nginx:1.27.0", "nginx:1.27.1", domain.UpdatePatch)
	// The same news again, as a second pass over an unchanged plan would be if
	// the planner's own suppression were ever removed.
	service.NotifyUpdateDiscovered(notifier, "web",
		"nginx:1.27.0", "nginx:1.27.1", domain.UpdatePatch)
	// Genuinely newer. Must NOT collide with the first.
	service.NotifyUpdateDiscovered(notifier, "web",
		"nginx:1.27.1", "nginx:1.28.0", domain.UpdateMinor)

	sent := notifier.all()
	if sent[0].DedupKey != sent[1].DedupKey {
		t.Errorf("the same available version produced two keys, %q and %q; a "+
			"cooldown could not collapse them", sent[0].DedupKey, sent[1].DedupKey)
	}
	if sent[0].DedupKey == sent[2].DedupKey {
		t.Errorf("a newer version reuses the key %q, so an operator who was told "+
			"about 1.27.1 is never told about 1.28.0", sent[0].DedupKey)
	}

	for _, notification := range sent {
		if notification.Severity != domain.NotifyInfo {
			t.Errorf("severity = %q; an available update is news, not a problem",
				notification.Severity)
		}
		// Nothing here may look like an instruction or an identifier a
		// monitor-only deployment could be led to act on.
		text := strings.ToLower(notification.Title + " " + notification.Body)
		for _, forbidden := range []string{"updating", "was updated", "recreat", "rolled"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("a discovery notification says %q:\n\t%s\n\t%s\n\n"+
					"Nothing has happened to the container. A monitor-only "+
					"deployment reading this must not think anything did.",
					forbidden, notification.Title, notification.Body)
			}
		}
	}
}
