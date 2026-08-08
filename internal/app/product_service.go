package app

import (
	"fmt"
	"strings"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/runner/internal/store"
)

type ProductService struct {
	store store.ProductStore
}

func NewProductService(productStore store.ProductStore) *ProductService {
	return &ProductService{store: productStore}
}

func (s *ProductService) ListProducts() ([]domain.ProductTemplate, error) {
	return s.store.List()
}

func (s *ProductService) GetProduct(name string) (domain.ProductTemplate, error) {
	return s.store.Get(name)
}

func (s *ProductService) SaveProduct(product domain.ProductTemplate) (domain.ProductTemplate, error) {
	normalized, err := ValidateProductTemplate(product)
	if err != nil {
		return domain.ProductTemplate{}, err
	}
	if err := s.store.Save(normalized); err != nil {
		return domain.ProductTemplate{}, err
	}
	return normalized, nil
}

func (s *ProductService) DeleteProduct(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "generic" {
		return ConflictError{Message: "generic product template cannot be deleted"}
	}
	return s.store.Delete(name)
}

func ValidateProductTemplate(product domain.ProductTemplate) (domain.ProductTemplate, error) {
	normalized, err := normalizeProductTemplate(product)
	if err != nil {
		return domain.ProductTemplate{}, err
	}
	if err := validateProductTemplateSchema(normalized); err != nil {
		return domain.ProductTemplate{}, err
	}
	return normalized, nil
}

func validateProductTemplateSchema(product domain.ProductTemplate) error {
	if strings.TrimSpace(product.Name) == "" {
		return ValidationError{Message: "product name is required"}
	}
	if product.ManifestSourceID == "" && strings.TrimSpace(product.BasePath) == "" {
		return ValidationError{Message: "manifestSourceId or basePath is required"}
	}
	if len(product.Services) == 0 {
		return ValidationError{Message: "at least one service template is required"}
	}
	for _, service := range product.Services {
		name := normalizeID(service.Name)
		if name == "" {
			return ValidationError{Message: "service name is required"}
		}
		service.TagKey = strings.TrimSpace(service.TagKey)
		if service.TagKey == "" {
			service.TagKey = defaultTagKey(name)
		}
		if strings.TrimSpace(service.DefaultTag) == "" {
			return ValidationError{Message: fmt.Sprintf("default tag is required for service %q", service.Name)}
		}
	}
	return nil
}

func normalizeProductTemplate(product domain.ProductTemplate) (domain.ProductTemplate, error) {
	product.Name = normalizeID(product.Name)
	if product.Name == "" {
		return domain.ProductTemplate{}, ValidationError{Message: "product name is required"}
	}
	if product.DefaultMode != "" && product.DefaultMode != domain.ModeFull && product.DefaultMode != domain.ModeHybrid {
		return domain.ProductTemplate{}, ValidationError{Message: fmt.Sprintf("unsupported default mode %q", product.DefaultMode)}
	}
	product.ManifestSourceID = normalizeID(product.ManifestSourceID)
	product.BasePath = strings.TrimSpace(product.BasePath)
	product.ValuesPath = strings.TrimSpace(product.ValuesPath)
	product.HealthCheck = strings.TrimSpace(product.HealthCheck)
	product.Project = normalizeID(product.Project)
	product.NamespaceSuffix = normalizeID(product.NamespaceSuffix)
	product.DefaultDomain = strings.TrimSpace(product.DefaultDomain)
	if product.Substitutions != nil {
		normalized := map[string]string{}
		for key, value := range product.Substitutions {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			normalized[key] = strings.TrimSpace(value)
		}
		product.Substitutions = normalized
	}
	product.Services = normalizeServiceTemplates(product.Services)
	if len(product.Services) == 0 {
		return domain.ProductTemplate{}, ValidationError{Message: "at least one service template is required"}
	}
	if product.ManifestSourceID == "" && product.BasePath == "" {
		return domain.ProductTemplate{}, ValidationError{Message: "manifestSourceId or basePath is required"}
	}
	return product, nil
}

func normalizeServiceTemplates(services []domain.ServiceTemplate) []domain.ServiceTemplate {
	normalized := make([]domain.ServiceTemplate, 0, len(services))
	seen := map[string]struct{}{}
	for _, service := range services {
		name := normalizeID(service.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		tagKey := strings.TrimSpace(service.TagKey)
		if tagKey == "" {
			tagKey = defaultTagKey(name)
		}
		defaultTag := strings.TrimSpace(service.DefaultTag)
		if defaultTag == "" {
			defaultTag = "latest"
		}
		normalized = append(normalized, domain.ServiceTemplate{
			Name:       name,
			TagKey:     tagKey,
			DefaultTag: defaultTag,
			Required:   service.Required,
		})
	}
	return normalized
}

func defaultTagKey(serviceName string) string {
	parts := strings.FieldsFunc(serviceName, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	if len(parts) == 0 {
		return "imageTag"
	}
	var builder strings.Builder
	builder.WriteString(parts[0])
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		builder.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			builder.WriteString(part[1:])
		}
	}
	builder.WriteString("Tag")
	return builder.String()
}
