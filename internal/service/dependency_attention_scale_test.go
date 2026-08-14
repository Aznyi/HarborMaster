package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
	"github.com/Aznyi/HarborMaster/internal/store"
)

// errSweepFailed stands in for the outstanding-member read failing.
var errSweepFailed = errors.New("the outstanding reattachment sweep failed")

// The attention read the container list and the dashboard share.
//
// # What is asserted, and what is merely reported
//
// The QUERY COUNT is asserted, because it is the property that decides whether
// rendering a page of two hundred containers costs three reads or six hundred.
// An N+1 over a small estate is fast, so a timing would never catch it.
//
// The timings are logged so an order-of-magnitude regression is visible, and
// nothing more is claimed for them: they run on whatever machine the suite does.

// countingOperationStore records the outstanding-member sweep.
//
// Wraps the counting dependency store so one fixture answers both halves of the
// attention read, and counts them separately so the two costs can be told apart.
type countingOperationStore struct {
	members []domain.DependencyMember
	sweeps  int
	err     error
}

func (s *countingOperationStore) UnsettledMembers(
	_ context.Context, _ int,
) ([]domain.DependencyMember, error) {
	s.sweeps++
	if s.err != nil {
		return nil, s.err
	}
	return append([]domain.DependencyMember(nil), s.members...), nil
}

// The rest of DependencyOperationStore, so this can be wired as one.
func (s *countingOperationStore) Open(context.Context, int) ([]domain.DependencyOperation, error) {
	return nil, nil
}

func (s *countingOperationStore) Get(context.Context, string) (domain.DependencyOperation, error) {
	return domain.DependencyOperation{}, nil
}

func (s *countingOperationStore) ActiveForProvider(
	context.Context, string,
) (domain.DependencyOperation, bool, error) {
	return domain.DependencyOperation{}, false, nil
}

func (s *countingOperationStore) Create(
	_ context.Context, operation domain.DependencyOperation, _ time.Time,
) (domain.DependencyOperation, error) {
	return operation, nil
}

func (s *countingOperationStore) AdvanceOperation(
	context.Context, string, domain.DependencyOperationState,
	domain.DependencyOperationFailure, time.Time,
) error {
	return nil
}

func (s *countingOperationStore) AdvanceMember(
	context.Context, store.MemberUpdate, time.Time,
) error {
	return nil
}

func (s *countingOperationStore) MembersForDependent(
	_ context.Context, dependent string,
) ([]domain.DependencyMember, error) {
	var out []domain.DependencyMember
	for _, member := range s.members {
		if member.Dependent == dependent {
			out = append(out, member)
		}
	}
	return out, nil
}

func (s *countingOperationStore) AttachProviderExecution(
	context.Context, string, string, time.Time,
) error {
	return nil
}

// The attention read at 25, 500 and 2,000 containers.
func TestDependencyAttentionCostsAFixedNumberOfQueries(t *testing.T) {
	t.Parallel()

	for _, size := range []int{25, 500, 2000} {
		t.Run(fmt.Sprintf("%d containers", size), func(t *testing.T) {
			t.Parallel()

			fixture := readScaleEstate(size)
			operations := &countingOperationStore{
				members: []domain.DependencyMember{
					{
						OperationID: "depop_0000000000000000abcd",
						Dependent:   "c00001",
						Provider:    "c00000",
						Source:      domain.DependencyNetworkNamespace,
						State:       domain.MemberFailed,
					},
				},
			}

			svc := service.NewDependencyService(service.DependencyOptions{
				Store:      fixture,
				Operations: operations,
			})

			started := time.Now()
			facts, err := svc.AttentionFacts(context.Background())
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("AttentionFacts at %d: %v", size, err)
			}

			// Two indexed reads for the graph, one bounded sweep for the
			// outstanding reattachments. Three, at every size.
			if fixture.queries() != 2 {
				t.Fatalf("the graph cost %d store queries, want 2", fixture.queries())
			}
			if operations.sweeps != 1 {
				t.Fatalf("the member sweep ran %d times, want 1\n"+
					"\tone sweep for the whole page is the difference between a "+
					"projection and an N+1", operations.sweeps)
			}

			// Non-vacuity: the fixture really is an estate of this size, and the
			// failed reattachment really did reach a container's facts.
			if len(facts) != size {
				t.Fatalf("facts cover %d containers, want %d", len(facts), size)
			}
			if !facts["c00001"].RebindFailed {
				t.Fatal("the failed reattachment did not reach the container it names")
			}
			if facts["c00002"].RebindFailed {
				t.Fatal("a reattachment reached a container it does not name")
			}

			// The dashboard summary is derived from the same facts, so the two
			// cannot disagree and it costs nothing extra.
			summarised := time.Now()
			summary := service.Summarise(facts)
			summariseElapsed := time.Since(summarised)
			if fixture.queries() != 2 || operations.sweeps != 1 {
				t.Fatal("summarising issued a query of its own")
			}
			if summary.RebindsFailed != 1 {
				t.Fatalf("summary.RebindsFailed = %d, want 1", summary.RebindsFailed)
			}

			t.Logf("%4d containers: attention %v (2 graph queries + 1 sweep), "+
				"summary %v (0 queries)", size, elapsed, summariseElapsed)
		})
	}
}

// A failed sweep understates rather than invents.
//
// The graph half is still correct and still worth returning. Reporting no
// reattachment when one could not be read is the safe direction for a DISPLAY
// path: the container's own page reads the operations directly, and every
// safety decision is made elsewhere by components that fail closed.
func TestAFailedMemberSweepStillReturnsTheGraphPicture(t *testing.T) {
	t.Parallel()

	fixture := readScaleEstate(25)
	operations := &countingOperationStore{err: errSweepFailed}

	svc := service.NewDependencyService(service.DependencyOptions{
		Store:      fixture,
		Operations: operations,
	})

	facts, err := svc.AttentionFacts(context.Background())
	if err != nil {
		t.Fatalf("AttentionFacts: %v", err)
	}
	if len(facts) != 25 {
		t.Fatalf("facts cover %d containers, want 25", len(facts))
	}
	for name, row := range facts {
		if row.RebindFailed || row.RebindPending {
			t.Errorf("%s reports a reattachment the sweep never returned", name)
		}
	}
}

// An estate past the node bound produces no facts at all.
//
// Fail closed on the DISPLAY read too: a partial graph makes containers look
// more independent than they are, and the caller renders the estate exactly as
// it did before dependencies existed rather than acting on a subset.
func TestAttentionFactsRefusePastTheNodeBound(t *testing.T) {
	t.Parallel()

	fixture := readScaleEstate(domain.MaxDependencyNodes + 1)
	svc := service.NewDependencyService(service.DependencyOptions{Store: fixture})

	if _, err := svc.AttentionFacts(context.Background()); err == nil {
		t.Fatal("AttentionFacts answered for an estate past the node bound")
	}
}
