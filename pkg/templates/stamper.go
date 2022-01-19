// Copyright 2021 VMware
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	yttcmd "github.com/k14s/ytt/pkg/cmd/template"
	yttui "github.com/k14s/ytt/pkg/cmd/ui"
	yttfiles "github.com/k14s/ytt/pkg/files"
	"github.com/valyala/fasttemplate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/vmware-tanzu/cartographer/pkg/apis/v1alpha1"
	"github.com/vmware-tanzu/cartographer/pkg/eval"
	"github.com/vmware-tanzu/cartographer/pkg/logger"
)

type Labels map[string]string

// JsonPathContext is any structure that you intend for jsonpath to treat as it's context.
// typically any struct with template-specific json structure tags
type JsonPathContext interface{}

type Stamper struct {
	TemplatingContext JsonPathContext
	Owner             client.Object
	Labels            Labels
}

func StamperBuilder(owner client.Object, templatingContext JsonPathContext, labels Labels) Stamper {
	return Stamper{
		TemplatingContext: templatingContext,
		Owner:             owner,
		Labels:            labels,
	}
}

type loopDetector []string

func (d loopDetector) checkItem(item string) (loopDetector, error) {
	var potentialLoop []string
	newStack := append(d, item)
	for _, currentItem := range newStack {
		if currentItem == item {
			potentialLoop = append(potentialLoop, currentItem)
		} else if len(potentialLoop) > 0 {
			potentialLoop = append(potentialLoop, currentItem)
		}
	}
	if len(potentialLoop) > 1 {
		return newStack, fmt.Errorf("infinite tag loop detected: %+v", formatExpressionLoop(potentialLoop))
	}

	return newStack, nil
}

func formatExpressionLoop(expressionLoop []string) string {
	result := ""
	for _, expression := range expressionLoop {
		if result != "" {
			result = result + " -> "
		}
		result = result + expression
	}
	return result
}

func (s *Stamper) recursivelyEvaluateTemplates(jsonValue interface{}, loopDetector loopDetector) (interface{}, error) {
	switch typedJSONValue := jsonValue.(type) {
	case string:
		stamperTagInterpolator := StandardTagInterpolator{
			Context:   s.TemplatingContext,
			Evaluator: eval.EvaluatorBuilder(),
		}
		loopDetector, err := loopDetector.checkItem(typedJSONValue)
		if err != nil {
			return nil, err
		}

		stampedLeafNode, err := InterpolateLeafNode(fasttemplate.ExecuteFuncStringWithErr, []byte(typedJSONValue), stamperTagInterpolator)
		if err != nil {
			return nil, fmt.Errorf("failed to interpolate leaf node: %w", err)
		}
		if jsonValue == stampedLeafNode {
			return stampedLeafNode, nil
		} else {
			return s.recursivelyEvaluateTemplates(stampedLeafNode, loopDetector)
		}
	case map[string]interface{}:
		stampedMap := make(map[string]interface{})
		for key, value := range typedJSONValue {
			stampedValue, err := s.recursivelyEvaluateTemplates(value, loopDetector)
			if err != nil {
				return nil, fmt.Errorf("failed to interpolate map value [%v]: %w", value, err)
			}
			stampedMap[key] = stampedValue
		}
		return stampedMap, nil
	case []interface{}:
		var stampedSlice []interface{}
		for _, sliceElement := range typedJSONValue {
			stampedElement, err := s.recursivelyEvaluateTemplates(sliceElement, loopDetector)
			if err != nil {
				return nil, fmt.Errorf("failed to interpolate array value [%v]: %w", sliceElement, err)
			}
			stampedSlice = append(stampedSlice, stampedElement)
		}
		return stampedSlice, nil
	default:
		return typedJSONValue, nil
	}
}

func (s *Stamper) Stamp(ctx context.Context, resourceTemplate v1alpha1.TemplateSpec) (*unstructured.Unstructured, error) {
	var stampedObject *unstructured.Unstructured
	var err error
	switch {
	case resourceTemplate.Template != nil:
		stampedObject, err = s.applyTemplate(resourceTemplate.Template.Raw)
	case resourceTemplate.Ytt != "":
		stampedObject, err = s.applyYtt(ctx, resourceTemplate.Ytt)
	default:
		err = fmt.Errorf("unknown resource template type, expected either template or ytt")
	}
	if err != nil {
		return nil, err
	}

	if stampedObject.GetNamespace() == "" {
		stampedObject.SetNamespace(s.Owner.GetNamespace())
	}

	apiVersion, kind := s.Owner.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	stampedObject.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         apiVersion,
			Kind:               kind,
			UID:                s.Owner.GetUID(),
			Name:               s.Owner.GetName(),
			BlockOwnerDeletion: pointer.BoolPtr(true),
			Controller:         pointer.BoolPtr(true),
		},
	})

	s.mergeLabels(stampedObject)

	return stampedObject, nil
}

func (s *Stamper) applyTemplate(resourceTemplateJSON []byte) (*unstructured.Unstructured, error) {
	var resourceTemplate interface{}
	err := json.Unmarshal(resourceTemplateJSON, &resourceTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal json resource template: %w", err)
	}

	stampedObjectJSON, err := s.recursivelyEvaluateTemplates(resourceTemplate, loopDetector{})
	if err != nil {
		return nil, fmt.Errorf("failed to recursively evaluate template: %w", err)
	}

	unstructuredContent, ok := stampedObjectJSON.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("stamped resource is not a map[string]interface{}, stamped resource: %+v", stampedObjectJSON)
	}
	stampedObject := &unstructured.Unstructured{}
	stampedObject.SetUnstructuredContent(unstructuredContent)

	return stampedObject, nil
}

func (s *Stamper) applyYtt(ctx context.Context, template string) (*unstructured.Unstructured, error) {
	log := logr.FromContextOrDiscard(ctx)

	yttOpts := yttcmd.NewOptions()

	// inject each key of the template context as a ytt value
	templateContext := map[string]interface{}{}
	b, err := json.Marshal(s.TemplatingContext)
	if err != nil {
		// NOTE we can ignore subsequent json errors, if there's an issue with the data it will be caught here
		return nil, fmt.Errorf("unable to marshal template context: %w", err)
	}
	err = json.Unmarshal(b, &templateContext)
	if err != nil {
		// NOTE this should never error because we have already checked the error after json.Marshal
		return nil, err
	}

	var dataValues []string
	for k := range templateContext {
		raw, _ := json.Marshal(templateContext[k])
		dataValues = append(dataValues, fmt.Sprintf("%s=%s", k, raw))
	}

	// equivalent to `--data-value-yaml`
	yttOpts.DataValuesFlags.KVsFromYAML = dataValues

	input := yttcmd.Input{
		Files: []*yttfiles.File{yttfiles.MustNewFileFromSource(yttfiles.NewBytesSource("stdin.yml", []byte(template)))},
	}
	noopUI := yttui.NewCustomWriterTTY(false, bytes.NewBuffer([]byte{}), bytes.NewBuffer([]byte{}))

	log.V(logger.DEBUG).Info("ytt call", "data values", dataValues, "input", template)
	output := yttOpts.RunWithFiles(input, noopUI)
	if output.Err != nil {
		return nil, fmt.Errorf("unable to apply ytt template: %w", output.Err)
	}

	outputBytes, err := output.DocSet.AsBytes()
	if err != nil {
		return nil, fmt.Errorf("unable to get output as bytes: %w", err)
	}
	log.V(logger.DEBUG).Info("ytt result", "output", string(outputBytes))

	stampedObject := &unstructured.Unstructured{}
	if err = yaml.Unmarshal(outputBytes, stampedObject); err != nil {
		// ytt should never return invalid yaml
		return nil, err
	}

	return stampedObject, nil
}

func (s *Stamper) mergeLabels(obj *unstructured.Unstructured) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	for key, value := range s.Labels {
		labels[key] = value
	}

	obj.SetLabels(labels)
}
