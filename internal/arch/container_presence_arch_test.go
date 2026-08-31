package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Presence is asked for by NAME, not remembered as a field check (C3E).
//
// # What went wrong
//
// The inventory keeps a container's row after it leaves the host, with
// present = 0, because history reads it. ContainerRepository.Get returns that
// row deliberately.
//
// planEvidence.ContainerPresent called Get and treated success as "the
// container exists". A container removed from the host therefore passed the
// presence gate standing in front of an image acquisition. The check was one
// line away, and the helper's name promised it had been done.
//
// # Why this is an interface rule rather than a grep
//
// A regex over call sites cannot tell a historical read from a current one --
// both are `containers.Get(ctx, id)` and only the surrounding intent differs.
// So the rule is enforced where intent already lives: the narrow interface each
// service declares for the container repository. A service that reasons about
// CURRENT state declares GetPresent and nothing else, and then cannot call Get
// even by accident, because its dependency does not have the method.
//
// This test pins that those interfaces stay narrow. It is parsed, not matched:
// it reads the declared method set rather than guessing from text.

// presentOnlyInterfaces are the container dependencies whose services reason
// about CURRENT state. Each must expose GetPresent and must NOT expose Get.
var presentOnlyInterfaces = map[string]string{
	"SnapshotContainers":   "captures a container's current configuration",
	"DriftContainers":      "compares a container's current configuration",
	"PolicyContainers":     "checks a container's current configuration",
	"DependencyContainers": "reports what a container is currently running",
}

func TestServicesThatNeedPresenceCannotAskTheHistoricalQuestion(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "internal", "service")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/service: %v", err)
	}

	fset := token.NewFileSet()
	found := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		{
			ast.Inspect(file, func(node ast.Node) bool {
				spec, ok := node.(*ast.TypeSpec)
				if !ok {
					return true
				}
				iface, ok := spec.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				if _, wanted := presentOnlyInterfaces[spec.Name.Name]; !wanted {
					return true
				}
				var methods []string
				for _, field := range iface.Methods.List {
					for _, name := range field.Names {
						methods = append(methods, name.Name)
					}
				}
				found[spec.Name.Name] = methods
				return true
			})
		}
	}

	for name, purpose := range presentOnlyInterfaces {
		methods, declared := found[name]
		if !declared {
			t.Errorf("%s is not declared in internal/service any more.\n\n"+
				"If it was renamed, rename it here too. If the service stopped "+
				"needing a container, remove the entry -- but do not delete this "+
				"guard to make a build pass: it is what stops a current-state "+
				"service reaching the historical record.", name)
			continue
		}

		var hasPresent, hasGet bool
		for _, method := range methods {
			switch method {
			case "GetPresent":
				hasPresent = true
			case "Get":
				hasGet = true
			}
		}

		if !hasPresent {
			t.Errorf("%s does not declare GetPresent, but it %s.\n\n"+
				"That question is about a container that EXISTS, and the "+
				"inventory keeps departed containers as rows.", name, purpose)
		}
		if hasGet {
			t.Errorf("%s declares Get, and it %s.\n\n"+
				"ContainerRepository.Get returns a container's record whether or "+
				"not the container is still on the host -- history depends on "+
				"that. A service reasoning about current state must not be able "+
				"to reach it: declare GetPresent alone, so the safe answer is "+
				"the only one available. This is exactly how a departed "+
				"container came to pass an acquisition presence gate.",
				name, purpose)
		}
	}
}
