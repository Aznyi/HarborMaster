package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Canonical image identity has exactly one producer (C3C).
//
// # What this protects
//
// domain.NormalizeImageRef decides what a reference MEANS: which registry it
// resolves to, which namespace is implied, whether it is acceptable at all. It
// refuses IP literals and ports, and it is the single origin of every network
// destination HarborMaster will contact.
//
// Migration 0033 stores its output on the container row so currency can be one
// join instead of a Go round trip. The value of that is entirely conditional on
// there being ONE canonicaliser: the moment a query starts assembling a
// canonical reference out of string functions, the schema contains a second,
// subtly different opinion about which registry gets contacted -- and it is the
// one no security review would think to look at.
//
// Migration 0015 is the precedent for what happens when SQL guesses at this
// mapping. It matched containers.image_ref against image_intel.reference, those
// never match, and every image reported zero containers affected for months.

// sqlCanonicalisation matches an attempt to BUILD a reference in SQL: string
// concatenation or substring surgery in the same statement as a reference
// column. Deliberately narrow -- comparing or selecting these columns is the
// whole point of the schema and must stay allowed.
var sqlCanonicalisation = regexp.MustCompile(
	`(?i)(SUBSTR|INSTR|REPLACE|LTRIM|RTRIM|PRINTF)\s*\(\s*[a-z_.]*image_(ref|canonical)` +
		`|image_(ref|canonical)\s*\|\|` +
		`|\|\|\s*[a-z_.]*image_(ref|canonical)` +
		`|'docker\.io/'\s*\|\|` +
		`|\|\|\s*'/library/'`)

func TestNoSQLBuildsACanonicalImageReference(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist", "data", "web":
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sql") {
			return nil
		}
		// This file carries the pattern it forbids.
		if name == "image_identity_arch_test.go" {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for index, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments explain the rule; they do not implement it.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue
			}
			if sqlCanonicalisation.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders,
					filepath.ToSlash(rel)+":"+itoa(index+1)+"  "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("SQL appears to construct a canonical image reference:\n\t%s\n\n"+
			"domain.NormalizeImageRef is the only canonicaliser. It decides which "+
			"registry HarborMaster contacts, and a second implementation in SQL "+
			"would be a second answer to that question. Store the value on the "+
			"write path -- see migration 0033 -- and join on it.",
			strings.Join(offenders, "\n\t"))
	}
}

// TestTheCanonicalColumnHasOneWriter pins that the derived column is produced
// in exactly one place.
//
// image_canonical is a projection of image_ref, and that only holds while a
// single statement maintains both. A second writer -- a repair routine, a
// backfill, a targeted update -- is how a derived column silently becomes an
// independent source of truth.
func TestTheCanonicalColumnHasOneWriter(t *testing.T) {
	root := moduleRoot(t)

	writes := regexp.MustCompile(`(?i)(INSERT INTO containers|UPDATE containers)`)
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "internal", "store"),
		func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			if !strings.HasSuffix(entry.Name(), ".go") ||
				strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if writes.MatchString(string(source)) &&
				strings.Contains(string(source), "image_canonical") {
				rel, _ := filepath.Rel(root, path)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(files) != 1 || files[0] != "internal/store/inventory_repository.go" {
		t.Errorf("image_canonical is written from %v, want only "+
			"internal/store/inventory_repository.go (upsertContainer).\n\n"+
			"The column is DERIVED from image_ref. One writer is what makes the "+
			"invariant hold; a second one turns a projection into a second "+
			"source of truth. See migration 0033.", files)
	}
}

// itoa avoids pulling strconv in for one call site.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
