package app

import (
	"envpilot/internal/domain"
	"testing"
)

func TestValidateProductTemplateSupportsDefaultsAndRequiresSchema(t *testing.T) {
	validated, err := ValidateProductTemplate(ProductTemplateFromTest())
	if err != nil {
		t.Fatalf("validate product: %v", err)
	}
	if validated.Name != "payments" {
		t.Fatalf("name = %q", validated.Name)
	}
	if validated.ManifestSourceID != "payments-template" {
		t.Fatalf("manifest source = %q", validated.ManifestSourceID)
	}
	if validated.Services[0].TagKey != "apiTag" {
		t.Fatalf("expected normalized tagKey, got %q", validated.Services[0].TagKey)
	}
}

func TestValidateProductTemplateRejectsMissingBindings(t *testing.T) {
	_, err := ValidateProductTemplate(ProductTemplateFromTestNoServices())
	if err == nil {
		t.Fatal("expected validation error for empty services")
	}
}

func TestValidateProductTemplateRejectsMissingMode(t *testing.T) {
	_, err := ValidateProductTemplate(ProductTemplateFromTestInvalidMode())
	if err == nil {
		t.Fatal("expected validation error for invalid mode")
	}
}

func ProductTemplateFromTest() domain.ProductTemplate {
	return domain.ProductTemplate{
		Name:            "payments",
		ManifestSourceID: "payments-template",
		BasePath:         "",
		DefaultMode:      domain.ModeHybrid,
		Services: []domain.ServiceTemplate{
			{Name: "api", TagKey: " apiTag ", DefaultTag: "latest"},
		},
	}
}

func ProductTemplateFromTestNoServices() domain.ProductTemplate {
	return domain.ProductTemplate{
		Name:       "payments",
		BasePath:   "charts/payments",
		DefaultMode: domain.ModeHybrid,
	}
}

func ProductTemplateFromTestInvalidMode() domain.ProductTemplate {
	return domain.ProductTemplate{
		Name:       "payments",
		BasePath:   "charts/payments",
		DefaultMode: "sidecar",
		Services:   []domain.ServiceTemplate{{Name: "api", DefaultTag: "latest"}},
	}
}
