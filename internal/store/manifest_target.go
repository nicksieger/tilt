package store

import (
	"fmt"
	"strings"

	"github.com/windmilleng/tilt/internal/ospath"
	"github.com/windmilleng/tilt/pkg/model"
)

type ManifestTarget struct {
	Manifest model.Manifest
	State    *ManifestState
}

func NewManifestTarget(m model.Manifest) *ManifestTarget {
	return &ManifestTarget{
		Manifest: m,
		State:    newManifestState(m.Name),
	}
}

func (t ManifestTarget) Spec() model.TargetSpec {
	return t.Manifest
}

func (t ManifestTarget) Status() model.TargetStatus {
	return t.State
}

var _ model.Target = &ManifestTarget{}

func (t *ManifestTarget) Facets(secrets model.SecretSet) []model.Facet {
	var ret []model.Facet

	if !t.Status().LastBuild().Empty() {
		ret = append(ret, model.Facet{
			Name:  "Last Build Log",
			Value: t.Status().LastBuild().Log.String(),
		})
	}

	if len(t.State.BuildHistory) != 0 {
		sb := strings.Builder{}
		histories := t.State.BuildHistory
		if len(histories) > 20 {
			histories = histories[:20]
		}
		for _, br := range histories {
			sb.WriteString("Build finished:\n")
			sb.WriteString(fmt.Sprintf("  Reason: %s\n", br.Reason.String()))
			sb.WriteString("  Result: ")
			if br.Error == nil {
				sb.WriteString("Success")
			} else {
				sb.WriteString(fmt.Sprintf("%v", br.Error))
			}
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("  Duration: %s\n", br.Duration().String()))
			if len(br.Edits) > 0 {
				edits := ospath.FileListDisplayNames(t.Manifest.LocalPaths(), br.Edits)
				sb.WriteString(fmt.Sprintf("  Changed files: %s\n", strings.Join(edits, ", ")))
			}
			sb.WriteString(("\n\n"))
		}

		ret = append(ret, model.Facet{
			Name:  "Build History",
			Value: sb.String(),
		})
	}

	if t.Manifest.IsK8s() {
		s := string(secrets.Scrub([]byte(t.Manifest.K8sTarget().YAML)))
		ret = append(ret, model.Facet{Name: "k8s_yaml", Value: s})
	}

	return ret
}
