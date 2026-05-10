package domain

type ProductTemplate struct {
	Name             string            `json:"name"`
	Project          string            `json:"project"`
	NamespaceSuffix  string            `json:"namespaceSuffix"`
	ManifestSourceID string            `json:"manifestSourceId,omitempty"`
	BasePath         string            `json:"basePath"`
	ValuesPath       string            `json:"valuesPath,omitempty"`
	TargetNamespace  string            `json:"targetNamespace,omitempty"`
	HealthCheck      string            `json:"healthCheck"`
	DefaultMode      EnvironmentMode   `json:"defaultMode"`
	DefaultDomain    string            `json:"defaultDomain"`
	DefaultCharts    ChartVersions     `json:"defaultCharts"`
	Infrastructure   Infrastructure    `json:"infrastructure"`
	Services         []ServiceTemplate `json:"services"`
	Substitutions    map[string]string `json:"substitutions,omitempty"`
}

type ServiceTemplate struct {
	Name       string `json:"name"`
	TagKey     string `json:"tagKey"`
	DefaultTag string `json:"defaultTag"`
	Required   bool   `json:"required"`
}
