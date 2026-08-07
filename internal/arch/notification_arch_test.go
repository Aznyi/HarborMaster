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

// Architecture tests for the notification subsystem.
//
// Three properties, each of which stops a whole class of leak rather than one
// instance of it:
//
//  1. Every notification SENTENCE is written in one file, so "does this carry a
//     secret" is a question with one place to look.
//  2. The credential type never reaches a package that renders or serves.
//  3. The engine holds no Docker capability, the same way the update engine
//     does not.

// notificationAuthor is the ONE file allowed to build a domain.Notification.
const notificationAuthor = "internal/service/notify_raise.go"

// notificationBuilders may also construct one, for reasons stated per file.
var notificationBuilders = map[string]string{
	// The engine builds the test notification, whose text is a constant, and
	// re-builds the stored one to hand to the sender.
	"internal/service/notification.go": "the engine's test send and its re-send of a stored delivery",
	// The domain owns the type and its sanitiser.
	"internal/domain/notification.go": "the type's own methods and tests",
}

// Every notification's words come from one file.
//
// The security property of this subsystem is a property of the SENTENCES: a
// notification must carry nothing but HarborMaster's own words, its own
// identifiers, and closed-vocabulary phrases. That is checkable when every
// sentence is in one file and unfalsifiable when a dozen services each compose
// their own.
func TestEveryNotificationIsWrittenInOnePlace(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	var offenders []string

	walkGoFiles(t, root, func(path string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, path)
		if relative == notificationAuthor {
			return
		}
		if _, allowed := notificationBuilders[relative]; allowed {
			return
		}
		if strings.HasSuffix(relative, "_test.go") {
			return
		}

		ast.Inspect(file, func(node ast.Node) bool {
			composite, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if !isSelector(composite.Type, "domain", "Notification") {
				return true
			}
			offenders = append(offenders, relative+":"+
				fset.Position(composite.Pos()).String())
			return true
		})
	})

	if len(offenders) > 0 {
		t.Fatalf("a domain.Notification is built outside %s:\n\t%s\n\n"+
			"Every notification's words are written in that one file so that "+
			"\"could this carry a secret\" has one place to look. A service that "+
			"composes its own sentence is a service that can put an environment "+
			"value or a Docker error in one. Add a NotifyX function there and "+
			"call it instead.",
			notificationAuthor, strings.Join(offenders, "\n\t"))
	}
}

// No notification is built from an error's text.
//
// A Docker error carries paths, mounts, and occasionally an environment value;
// a registry error carries the URL, which for a private registry carries the
// host and sometimes the credential. Every failure a notification describes is
// described by the closed vocabulary the pipeline already produced for it.
func TestNoNotificationCarriesAnErrorsText(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(notificationAuthor))

	// Parsed rather than grepped, so the file's own comment SAYING never to do
	// this is not mistaken for doing it.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", notificationAuthor, err)
	}

	var offenders []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			// Any `x.Error()`, and any formatting call at all: fmt.Sprintf with
			// %w or %v is exactly how an error's text gets into a sentence.
			selector, ok := typed.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "Error" && len(typed.Args) == 0 {
				offenders = append(offenders, "an error's Error() at "+
					fset.Position(typed.Pos()).String())
			}
			if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Name == "fmt" {
				offenders = append(offenders, "a fmt call at "+
					fset.Position(typed.Pos()).String())
			}
		case *ast.BasicLit:
			if typed.Kind != token.STRING {
				return true
			}
			for _, verb := range []string{"%w", "%v", "%s", "%q"} {
				if strings.Contains(typed.Value, verb) {
					offenders = append(offenders, "a format verb "+verb+" at "+
						fset.Position(typed.Pos()).String())
				}
			}
		}
		return true
	})

	if len(offenders) > 0 {
		t.Fatalf("%s interpolates:\n\t%s\n\n"+
			"Notification text is HarborMaster's own words and closed-vocabulary "+
			"phrases only, composed by concatenation from values the caller already "+
			"holds. An error's text is neither, and the ones this codebase handles "+
			"carry registry URLs, container paths, and environment values. A format "+
			"call is the mechanism by which one gets in.",
			notificationAuthor, strings.Join(offenders, "\n\t"))
	}
}

// A credential may be CONSTRUCTED outside the trusted packages, never received.
//
// domain.NotificationSecret is a separate type from the destination precisely so
// a handler that never loads one cannot leak one. But a credential has to be
// accepted somewhere: the create and edit endpoints take a webhook URL in the
// request body, and that URL has to become a NotificationSecret before it can
// travel inward.
//
// So the rule is DIRECTIONAL. Outside the packages that legitimately hold one,
// the type may appear only inside a composite literal -- building one to pass
// in. Any other appearance is a value coming BACK: a variable declaration, a
// function result, a struct field. Those are what leak.
func TestACredentialIsOnlyEverConstructedOutsideTheTrustedPackages(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	// Where a credential legitimately lives: the type itself, the repository
	// that stores it, the engine that reads it immediately before a send, and
	// the transport that uses it.
	holders := map[string]bool{
		"internal/domain":  true,
		"internal/store":   true,
		"internal/service": true,
		"internal/notify":  true,
	}

	var offenders []string
	walkGoFiles(t, root, func(_ string, file *ast.File, fset *token.FileSet) {
		relative := relativeSlash(t, root, fset.Position(file.Pos()).Filename)
		if strings.HasSuffix(relative, "_test.go") {
			return
		}
		if holders[filepath.ToSlash(filepath.Dir(relative))] {
			return
		}

		// The INWARD positions: building one, and declaring a parameter that
		// takes one. Everything else the type is named in is a value arriving
		// back, which is what leaks.
		inward := map[token.Pos]struct{}{}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CompositeLit:
				// domain.NotificationSecret{...}, and &domain.NotificationSecret{...},
				// which parses to the same composite.
				if selector, ok := typed.Type.(*ast.SelectorExpr); ok &&
					isSelector(selector, "domain", "NotificationSecret") {
					inward[selector.Pos()] = struct{}{}
				}
			case *ast.FuncType:
				// A parameter takes a credential INWARD. A RESULT hands one
				// back, and typed.Results is deliberately not walked here.
				if typed.Params == nil {
					return true
				}
				for _, param := range typed.Params.List {
					if selector, ok := param.Type.(*ast.SelectorExpr); ok &&
						isSelector(selector, "domain", "NotificationSecret") {
						inward[selector.Pos()] = struct{}{}
					}
				}
			}
			return true
		})

		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || !isSelector(selector, "domain", "NotificationSecret") {
				return true
			}
			if _, allowed := inward[selector.Pos()]; allowed {
				return true
			}
			offenders = append(offenders, fset.Position(selector.Pos()).String())
			return true
		})
	})

	if len(offenders) > 0 {
		t.Fatalf("domain.NotificationSecret appears outside a construction, outside "+
			"the packages that may hold one:\n\t%s\n\n"+
			"A credential may travel INWARD -- an endpoint accepts a webhook URL and "+
			"builds one. It must never travel back out. Serve "+
			"domain.NotificationDestination, whose Endpoint is a scheme and host.",
			strings.Join(offenders, "\n\t"))
	}
}

// isSelector reports whether an expression is `pkg.Name`.
func isSelector(expr ast.Expr, pkg, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == pkg
}

// relativeSlash renders a path relative to the repository root with forward
// slashes, so a comparison reads the same on every platform.
func relativeSlash(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relative path for %s: %v", path, err)
	}
	return filepath.ToSlash(relative)
}

// walkGoFiles parses every non-vendored Go file under root.
func walkGoFiles(t *testing.T, root string, visit func(string, *ast.File, *token.FileSet)) {
	t.Helper()

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "web", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		visit(path, parsed, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
}
