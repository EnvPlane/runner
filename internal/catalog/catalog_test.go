package catalog

import "testing"

func TestDefaultCatalogProductsAreBootstrapValid(t *testing.T) {
	for name, product := range Default().Products {
		if product.Name == "" {
			t.Fatalf("default product %q must have a name", name)
		}
	}
}
