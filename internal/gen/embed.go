package gen

import (
	_ "embed"

	"github.com/hostryhq/go-auth"
)

//go:embed catalogue.json
var catalogueJSON []byte

// Catalogue returns the permission catalogue generated from api-specs/common/permissions.yaml.
func Catalogue() (*auth.Catalogue, error) { return auth.LoadCatalogue(catalogueJSON) }
