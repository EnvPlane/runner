package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/envpilot/contracts/domain"
)

type Catalog struct {
	Products map[string]domain.ProductTemplate `json:"products"`
}

func Load(path string) (Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return Default(), nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Catalog{}, err
	}
	var catalog Catalog
	if err := json.Unmarshal(content, &catalog); err != nil {
		return Catalog{}, err
	}
	if len(catalog.Products) == 0 {
		return Default(), nil
	}
	for key, product := range catalog.Products {
		product.Name = strings.TrimSpace(product.Name)
		if product.Name == "" {
			product.Name = key
		}
		catalog.Products[key] = product
	}
	return catalog, nil
}

func Default() Catalog {
	return Catalog{
		Products: map[string]domain.ProductTemplate{
			"generic": {
				Name: "generic",
				Infrastructure: domain.Infrastructure{
					Zone:     "default",
					Capacity: "standard",
				},
				Services: []domain.ServiceTemplate{
					{Name: "app", TagKey: "imageTag", DefaultTag: "latest", Required: true},
				},
			},
			"bethunder": {
				Name:            "bethunder",
				Project:         "cms",
				NamespaceSuffix: "cms",
				BasePath:        "common/apps/bethunder",
				HealthCheck:     "nginx",
				DefaultMode:     domain.ModeHybrid,
				DefaultDomain:   "feature.int",
				DefaultCharts: domain.ChartVersions{
					App:   "${appChartVersion}",
					Infra: "${infraChartVersion}",
					Nginx: "${nginxChartVersion}",
				},
				Infrastructure: domain.Infrastructure{
					MySQL:     true,
					RabbitMQ:  true,
					Redis:     true,
					Memcached: true,
					MongoDB:   true,
					Zone:      "ca-central-1b",
					Capacity:  "spot",
				},
				Services: []domain.ServiceTemplate{
					{Name: "nginx", TagKey: "nginxTag", DefaultTag: "latest", Required: true},
					{Name: "php", TagKey: "phpTag", DefaultTag: "latest", Required: true},
					{Name: "nuxt", TagKey: "nuxtTag", DefaultTag: "latest"},
					{Name: "betting", TagKey: "bettingTag", DefaultTag: "latest"},
					{Name: "websockets", TagKey: "websocketsTag", DefaultTag: "latest"},
					{Name: "api", TagKey: "cmsApiTag", DefaultTag: "latest"},
					{Name: "backend", TagKey: "cmsBackendTag", DefaultTag: "latest"},
					{Name: "frontend", TagKey: "cmsFrontendTag", DefaultTag: "latest"},
					{Name: "migration", TagKey: "cmsMigrationTag", DefaultTag: "latest"},
					{Name: "rest", TagKey: "cmsRestTag", DefaultTag: "latest"},
					{Name: "notifications", TagKey: "apiNotificationTag", DefaultTag: "latest"},
					{Name: "heimdall", TagKey: "apiHeimdallTag", DefaultTag: "latest"},
					{Name: "cursus", TagKey: "apiCursusTag", DefaultTag: "latest"},
					{Name: "iris", TagKey: "apiIrisTag", DefaultTag: "latest"},
				},
			},
		},
	}
}

func (c Catalog) Get(name string) (domain.ProductTemplate, bool) {
	product, ok := c.Products[strings.ToLower(strings.TrimSpace(name))]
	return product, ok
}

func (c Catalog) List() []domain.ProductTemplate {
	items := make([]domain.ProductTemplate, 0, len(c.Products))
	for _, product := range c.Products {
		items = append(items, product)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})
	return items
}
