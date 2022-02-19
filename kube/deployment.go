package kube

import (
	"errors"
	"fmt"

	"github.com/vikramraodp/fissile/helm"
	"github.com/vikramraodp/fissile/model"
	"github.com/vikramraodp/fissile/util"
)

// NewDeployment creates a Deployment for the given instance group, and its attached services
func NewDeployment(instanceGroup *model.InstanceGroup, settings ExportSettings, grapher util.ModelGrapher) (helm.Node, helm.Node, error) {
	podTemplate, err := NewPodTemplate(instanceGroup, settings, grapher)
	if err != nil {
		return nil, nil, err
	}

	svc, err := NewServiceList(instanceGroup, false, settings)
	if err != nil {
		return nil, nil, err
	}
	spec := helm.NewMapping()
	spec.Add("selector", newSelector(instanceGroup, settings))
	spec.Add("template", podTemplate)

	cb := NewConfigBuilder().
		SetSettings(&settings).
		SetConditionalAPIVersion("apps/v1", "extensions/v1beta1").
		SetKind("Deployment").
		SetName(instanceGroup.Name).
		AddModifier(helm.Comment(instanceGroup.GetLongDescription()))
	deployment, err := cb.Build()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build a new kube config: %v", err)
	}
	deployment.Add("spec", spec)
	addFeatureCheck(instanceGroup, deployment, svc)
	err = replicaCheck(instanceGroup, deployment, settings)
	if err != nil {
		return nil, nil, err
	}
	err = generalCheck(instanceGroup, deployment, settings)
	return deployment, svc, err
}

// getAffinityBlock returns an affinity block to add to a podspec
func getAffinityBlock(instanceGroup *model.InstanceGroup) *helm.Mapping {
	affinity := helm.NewMapping()

	if instanceGroup.Run != nil && instanceGroup.Run.Affinity != nil && instanceGroup.Run.Affinity.PodAntiAffinity != nil {
		// Add pod anti affinity from role manifest
		affinity.Add("podAntiAffinity", instanceGroup.Run.Affinity.PodAntiAffinity)
	}

	// Add node affinity template to be filled in by values.yaml
	roleName := makeVarName(instanceGroup.Name)
	nodeCond := fmt.Sprintf("if .Values.sizing.%s.affinity.nodeAffinity", roleName)
	nodeAffinity := fmt.Sprintf("{{ toJson .Values.sizing.%s.affinity.nodeAffinity }}", roleName)
	affinity.Add("nodeAffinity", nodeAffinity, helm.Block(nodeCond))

	return affinity
}

// addAffinityRules adds affinity rules to the pod spec
func addAffinityRules(instanceGroup *model.InstanceGroup, spec *helm.Mapping, settings ExportSettings) error {
	if instanceGroup.Run.Affinity != nil {
		if instanceGroup.Run.Affinity.NodeAffinity != nil {
			return errors.New("node affinity in role manifest not allowed")
		}

		if instanceGroup.Run.Affinity.PodAffinity != nil {
			return errors.New("pod affinity in role manifest not supported")
		}
	}

	if settings.CreateHelmChart {
		podSpec := spec.Get("template", "spec").(*helm.Mapping)

		podSpec.Add("affinity", getAffinityBlock(instanceGroup))
		podSpec.Sort()
	}

	meta := spec.Get("template", "metadata").(*helm.Mapping)
	if meta.Get("annotations") == nil {
		meta.Add("annotations", helm.NewMapping())
		meta.Sort()
	}
	annotations := meta.Get("annotations").(*helm.Mapping)

	annotations.Sort()

	return nil
}

// generalCheck adds common guards to the pod described by the
// controller. This only applies to helm charts, not basic kube
// definitions.
func generalCheck(instanceGroup *model.InstanceGroup, controller *helm.Mapping, settings ExportSettings) error {
	if !settings.CreateHelmChart {
		return nil
	}

	// The global config keys found under `sizing` in
	// `values.yaml` (HA, cpu, memory) were moved out of that
	// hierarchy into `config`. This gives `sizing` a uniform
	// structure, containing only the per-instance-group descriptions. It
	// also means that we now have to guard ourselves against use
	// of the old keys. Here we add the necessary guard
	// conditions.
	//
	// Note. The construction shown for requests and limits is used because of
	// (1) When FOO is nil you cannot check for FOO.BAR, for any BAR.
	// (2) The `and` operator is not short cuircuited, it evals all of its arguments
	// Thus `and FOO FOO.BAR` will not work either

	fail := `{{ fail "Bad use of moved variable sizing.HA. The new name to use is config.HA" }}`
	controller.Add("_moved_sizing_HA", fail, helm.Block("if .Values.sizing.HA"))

	for _, key := range []string{
		"cpu",
		"memory",
	} {
		// requests, limits - More complex to avoid limitations of the go templating system.
		// Guard on the main variable and then use a guarded value for the child.
		// The else branch is present in case we happen to get instance groups named `cpu` or `memory`.

		for _, subkey := range []string{
			"limits",
			"requests",
		} {
			guardVariable := fmt.Sprintf("_moved_sizing_%s_%s", key, subkey)
			block := fmt.Sprintf("if .Values.sizing.%s", key)
			fail := fmt.Sprintf(`{{ if .Values.sizing.%s.%s }} {{ fail "Bad use of moved variable sizing.%s.%s. The new name to use is config.%s.%s" }} {{else}} ok {{end}}`,
				key, subkey, key, subkey, key, subkey)
			controller.Add(guardVariable, fail, helm.Block(block))
		}
	}

	controller.Sort()
	return nil
}

// addFeatureCheck adds a conditional if a role is dependent on a feature flag,
// such that the nodes will only be included when the feature is enabled.
func addFeatureCheck(instanceGroup *model.InstanceGroup, nodes ...helm.Node) {
	// default_feature, if_feature, and unless_feature are all mutually exclusive, so only one can be set
	var nodeMod helm.NodeModifier
	if instanceGroup.IfFeature != "" {
		nodeMod = helm.Block(fmt.Sprintf("if .Values.enable.%s", instanceGroup.IfFeature))
	} else if instanceGroup.DefaultFeature != "" {
		nodeMod = helm.Block(fmt.Sprintf("if .Values.enable.%s", instanceGroup.DefaultFeature))
	} else if instanceGroup.UnlessFeature != "" {
		nodeMod = helm.Block(fmt.Sprintf("if not .Values.enable.%s", instanceGroup.UnlessFeature))
	}
	if nodeMod != nil {
		for _, node := range nodes {
			if node != nil {
				node.Set(nodeMod)
			}
		}
	}

}

func notNil(variable string) string {
	return fmt.Sprintf(`(ne (typeOf %s) "<nil>")`, variable)
}

func replicaCount(instanceGroup *model.InstanceGroup, quoted bool) string {
	quote := ""
	if quoted {
		quote = " | quote"
	}
	count := fmt.Sprintf(".Values.sizing.%s.count", makeVarName(instanceGroup.Name))
	return fmt.Sprintf(`{{ if %s }}{{ %s%s }}{{ else }}`+
		`{{ if .Values.config.HA }}{{ %d%s }}{{ else }}{{ %d%s }}{{ end }}{{ end }}`,
		notNil(count), count, quote,
		instanceGroup.Run.Scaling.HA, quote, instanceGroup.Run.Scaling.Min, quote)
}

// replicaCheck adds various guards to validate the number of replicas
// for the pod described by the controller. It further adds the
// replicas specification itself as well.
func replicaCheck(instanceGroup *model.InstanceGroup, controller *helm.Mapping, settings ExportSettings) error {
	spec := controller.Get("spec").(*helm.Mapping)

	err := addAffinityRules(instanceGroup, spec, settings)
	if err != nil {
		return err
	}

	if !settings.CreateHelmChart {
		spec.Add("replicas", instanceGroup.Run.Scaling.Min)
		spec.Sort()
		return nil
	}

	spec.Add("replicas", replicaCount(instanceGroup, false))
	spec.Sort()

	roleName := makeVarName(instanceGroup.Name)
	count := fmt.Sprintf(".Values.sizing.%s.count", roleName)

	// min replica check
	fail := fmt.Sprintf(`{{ fail "%s must have at least %d instances" }}`, roleName, instanceGroup.Run.Scaling.Min)
	block := fmt.Sprintf("if and %s (lt (int %s) %d)", notNil(count), count, instanceGroup.Run.Scaling.Min)
	controller.Add("_minReplicas", fail, helm.Block(block))

	// min HA replica check
	fail = fmt.Sprintf(`{{ fail "%s must have at least %d instances for HA" }}`, roleName, instanceGroup.Run.Scaling.HA)
	block = fmt.Sprintf("if and .Values.config.HA .Values.config.HA_strict %s (lt (int %s) %d)",
		notNil(count), count, instanceGroup.Run.Scaling.HA)
	controller.Add("_minHAReplicas", fail, helm.Block(block))

	// max replica check
	fail = fmt.Sprintf(`{{ fail "%s cannot have more than %d instances" }}`, roleName, instanceGroup.Run.Scaling.Max)
	block = fmt.Sprintf("if and %s (gt (int %s) %d)", notNil(count), count, instanceGroup.Run.Scaling.Max)
	controller.Add("_maxReplicas", fail, helm.Block(block))

	// odd replica check
	if instanceGroup.Run.Scaling.MustBeOdd {
		fail := fmt.Sprintf(`{{ fail "%s must have an odd instance count" }}`, roleName)
		block := fmt.Sprintf("if and %s (eq (mod (int %s) 2) 0)", notNil(count), count)
		controller.Add("_oddReplicas", fail, helm.Block(block))
	}

	controller.Sort()

	return nil
}
