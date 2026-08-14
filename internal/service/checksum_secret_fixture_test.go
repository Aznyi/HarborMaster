package service_test

import (
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
	"github.com/Aznyi/HarborMaster/internal/service"
)

// The external-package twin of the fixture helpers in
// snapshot_secret_fixture_test.go.
//
// Duplicated rather than exported: these exist only so a fixture secret has the
// same shape a real one does -- classified and digested by one masker, because
// the comparison evidence is computed from the value at classification and
// cannot be reconstructed afterwards. Exporting a test-only constructor from
// the production package to save a dozen lines would be the worse trade.

var (
	checksumMaskerOnce sync.Once
	checksumMaskerImpl *domain.Masker
)

func checksumFixtureMasker() *domain.Masker {
	checksumMaskerOnce.Do(func() {
		key, err := service.LoadSecretKey(service.SecretKeyOptions{
			Value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		})
		if err != nil {
			panic("fixture secret key: " + err.Error())
		}
		checksumMaskerImpl = domain.NewMasker(domain.DefaultMaskPatterns).
			WithDigester(service.NewHasher(key).DigestValue)
	})
	return checksumMaskerImpl
}

// fixtureSecret builds one sensitive variable, digest and all.
func fixtureSecret(name, value string) domain.EnvVar {
	return checksumFixtureMasker().Classify(name, value)
}

// setFixtureSecret re-classifies a variable, as the daemon would report a
// rotated credential.
func setFixtureSecret(vars []domain.EnvVar, name, value string) {
	for i := range vars {
		if vars[i].Name == name {
			vars[i] = checksumFixtureMasker().Classify(name, value)
			return
		}
	}
}
