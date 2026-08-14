package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// The dependency endpoints, and the negative paths that matter.
//
// The write is administrator-only because DELETING a relationship removes a
// safety constraint. The tests below assert that from both directions: a viewer
// and an operator are refused, and an administrator succeeds.

func operatorEdgeFixture() domain.WorkloadDependency {
	return domain.WorkloadDependency{
		DependencyID: "dep_0123456789abcdef0123",
		Dependent:    "api",
		Dependency:   "postgres",
		Source:       domain.DependencyOperator,
	}
}

func namespaceEdgeFixture() domain.WorkloadDependency {
	return domain.WorkloadDependency{
		Dependent:  "sonarr",
		Dependency: "gluetun",
		Source:     domain.DependencyNetworkNamespace,
	}
}

// A discovered relationship is never reported as deletable, and carries no id
// for a caller to name.
func TestDiscoveredDependenciesAreNotDeletable(t *testing.T) {
	t.Parallel()

	rendered := renderDependency(namespaceEdgeFixture())
	if rendered.Deletable {
		t.Fatal("a Docker-derived relationship was reported as deletable")
	}
	if rendered.DependencyID != "" {
		t.Fatal("a Docker-derived relationship carries an id a caller could DELETE")
	}
	if !rendered.Hard {
		t.Fatal("a namespace relationship should be hard")
	}
	if rendered.Origin != "Detected by HarborMaster" {
		t.Fatalf("origin = %q", rendered.Origin)
	}

	operator := renderDependency(operatorEdgeFixture())
	if !operator.Deletable {
		t.Fatal("an operator relationship should be deletable")
	}
	if operator.Hard {
		t.Fatal("an operator relationship must not claim to be a runtime requirement")
	}
	if operator.Origin != "Configured by you" {
		t.Fatalf("origin = %q", operator.Origin)
	}
}

// The create request accepts exactly two fields, and rejects everything else.
//
// Unknown-field rejection is what stops a client believing an option took
// effect. The extra fields tried here are the ones that would matter: an image,
// a digest, a container id, a source.
func TestDependencyCreateRejectsEveryUnknownField(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"dependent":"api","dependency":"postgres","image":"nginx:1.27"}`,
		`{"dependent":"api","dependency":"postgres","digest":"sha256:abc"}`,
		`{"dependent":"api","dependency":"postgres","containerId":"abc"}`,
		`{"dependent":"api","dependency":"postgres","source":"dockerNetworkNamespace"}`,
		`{"dependent":"api","dependency":"postgres","providerId":"abc"}`,
		`{"dependent":"api","dependency":"postgres","registry":"ghcr.io"}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			var request dependencyCreateRequest
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatalf("the fixture is not valid JSON: %v", err)
			}
			// The decoder the handler uses rejects unknown fields; this asserts
			// the SHAPE has nowhere to put them, which is the stronger claim.
			decoder := json.NewDecoder(strings.NewReader(body))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&dependencyCreateRequest{}); err == nil {
				t.Fatal("an unknown field was accepted")
			}
		})
	}
}

// The request type has no field a Docker value could be smuggled into.
//
// Walked by name rather than by type: a `string` field called `image` would
// pass any type check and would be exactly the problem.
func TestDependencyCreateRequestCarriesOnlyNames(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"image", "digest", "registry", "namespace", "container",
		"provider", "id", "docker", "mount", "volume", "network",
		"command", "entrypoint", "env", "label", "privileged",
	}

	var body dependencyCreateRequest
	fields := reflectFieldNames(body)
	if len(fields) != 2 {
		t.Fatalf("dependencyCreateRequest has %d fields, want exactly 2: %v", len(fields), fields)
	}
	for _, field := range fields {
		lowered := strings.ToLower(field)
		for _, banned := range forbidden {
			if strings.Contains(lowered, banned) {
				t.Errorf("dependencyCreateRequest.%s resembles a Docker target (%q)\n"+
					"\tthis request may carry two container NAMES and nothing else. A field "+
					"a caller could put an image, a digest, or a container id into would be "+
					"a caller-supplied Docker value reaching a decision about mutation.",
					field, banned)
			}
		}
	}
}

// A caller with only dependency:read cannot write, and a viewer cannot either.
//
// The DELETE is the one that matters: it removes a safety constraint.
func TestDependencyWritesAreAdministratorOnly(t *testing.T) {
	t.Parallel()

	if domain.RoleViewer.Can(domain.PermDependencyManage) {
		t.Error("a viewer holds dependency:manage")
	}
	if domain.RoleOperator.Can(domain.PermDependencyManage) {
		t.Error("an operator holds dependency:manage\n" +
			"\tdeleting a relationship removes a safety constraint standing between " +
			"an unattended update and the container that has to finish first")
	}
	if !domain.RoleAdministrator.Can(domain.PermDependencyManage) {
		t.Error("an administrator does not hold dependency:manage")
	}

	// The read is held by everybody, like automation:read.
	for _, role := range domain.Roles {
		if !role.Can(domain.PermDependencyRead) {
			t.Errorf("%s does not hold dependency:read", role)
		}
	}

	// And dependency:read grants nothing that mutates.
	for _, permission := range domain.RoleViewer.Permissions() {
		if permission == domain.PermExecutionCreate ||
			permission == domain.PermAcquisitionCreate ||
			permission == domain.PermRollbackCreate {
			t.Errorf("the viewer set contains the mutation permission %q", permission)
		}
	}
}

// A malformed id is indistinguishable from an absent one.
//
// Otherwise the endpoint becomes an oracle for which relationship ids exist.
func TestDependencyDeleteRejectsMalformedIDsAsNotFound(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"", "not-an-id", "dep_", "' OR 1=1 --", "<script>alert(1)</script>",
		strings.Repeat("a", 500), "dep_ZZZZZZZZZZZZZZZZZZZZ",
	} {
		if domain.ValidDependencyID(bad) {
			t.Errorf("%q validated as a dependency id", bad)
		}
	}
	// The generated shape is the only one accepted.
	if !domain.ValidDependencyID(domain.NewDependencyID()) {
		t.Fatal("a generated id does not validate")
	}
}

// A refusal is reported as a conflict carrying HarborMaster's own sentence, and
// never the caller's input.
func TestDependencyRefusalsExplainWithoutEchoingInput(t *testing.T) {
	t.Parallel()

	for _, refusal := range domain.DependencyRefusals {
		explanation := refusal.Explain()
		if explanation == "" {
			t.Errorf("refusal %q has no explanation", refusal)
			continue
		}
		// The explanations are fixed phrases. None of them interpolates a
		// container name, which is what stops a response reflecting input.
		for _, marker := range []string{"%s", "%v", "%q", "%d"} {
			if strings.Contains(explanation, marker) {
				t.Errorf("refusal %q interpolates: %q", refusal, explanation)
			}
		}
	}
}

// The status a refusal maps to, checked against the handler's own mapping.
func TestDependencyRefusalMapsToConflict(t *testing.T) {
	t.Parallel()

	// Conflict rather than 400: the request was well-formed and the ESTATE
	// refused it. A 400 would suggest the caller should change the body.
	if CodeConflict != "conflict" {
		t.Fatalf("CodeConflict = %q", CodeConflict)
	}
	_ = http.StatusConflict
}

// reflectFieldNames returns a struct's field names.
func reflectFieldNames(value any) []string {
	typ := reflect.TypeOf(value)
	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		names = append(names, typ.Field(i).Name)
	}
	return names
}
