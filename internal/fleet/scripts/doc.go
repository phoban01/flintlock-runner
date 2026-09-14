// Package scripts renders the Fleet Controller's provisioning scripts
// (docs/requirements/06-fleet.md). Every script is a bash template embedded
// with go:embed and rendered from a fleet.RenderInput; CI renders each with a
// representative input and runs `bash -n` and shellcheck over it (TD-043).
package scripts
