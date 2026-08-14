package service

import (
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// Building sensitive fixture variables the way production builds them.
//
// # Why a helper rather than a struct literal
//
// A sensitive EnvVar carries comparison evidence that is computed AT
// CLASSIFICATION, from the value, while it is still in memory -- see
// domain.EnvVar.Digest. Nothing downstream can reconstruct it, because the
// value is gone by then; that is the whole point of the design and was the
// defect when the evidence was computed later.
//
// So a fixture that sets Name/Value/RawValue by hand describes a variable that
// production cannot produce: masked, but with no evidence attached. Tests built
// that way would assert on a shape the real system never has. These helpers
// route through the same masker the composition root wires, so a fixture secret
// and a real one differ only in their value.

var (
	fixtureMaskerOnce sync.Once
	fixtureMaskerImpl *domain.Masker
)

// fixtureMasker is the masker as main wires it: classifying and digesting.
func fixtureMasker() *domain.Masker {
	fixtureMaskerOnce.Do(func() {
		key, err := LoadSecretKey(SecretKeyOptions{
			Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		})
		if err != nil {
			// A fixture key that will not load is a broken test binary, not a
			// test failure worth reporting per-test.
			panic("fixture secret key: " + err.Error())
		}
		fixtureMaskerImpl = domain.NewMasker(domain.DefaultMaskPatterns).
			WithDigester(NewHasher(key).DigestValue)
	})
	return fixtureMaskerImpl
}

// fixtureSecret builds one sensitive variable, digest and all.
func fixtureSecret(name, value string) domain.EnvVar {
	return fixtureMasker().Classify(name, value)
}

// setFixtureSecret replaces a variable's value as the daemon would report a
// rotated credential: re-classified, so the evidence moves with it.
//
// Silently does nothing if the name is absent, which a caller notices as the
// assertion it was about to make failing.
func setFixtureSecret(vars []domain.EnvVar, name, value string) {
	for i := range vars {
		if vars[i].Name == name {
			vars[i] = fixtureMasker().Classify(name, value)
			return
		}
	}
}
