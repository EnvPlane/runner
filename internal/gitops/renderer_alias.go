package gitops

import "github.com/envplane/gitops/render"

// Renderer types are aliases to the canonical gitops module. Keep this
// compatibility package while callers migrate away from internal imports.
type FluxOptions = render.FluxOptions
type Renderer = render.Renderer
type Manifest = render.Manifest
type FluxRenderer = render.FluxRenderer

func NewFluxRenderer(options FluxOptions) FluxRenderer { return render.NewFluxRenderer(options) }
func ValuesYAML(values map[string]string) string       { return render.ValuesYAML(values) }
func NamespaceName(id string) string                   { return render.NamespaceName(id) }
