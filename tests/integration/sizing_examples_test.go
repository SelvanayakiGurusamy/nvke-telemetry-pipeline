// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// TestSizingExamples checks independent coverage, baseline compatibility and
// ordinary Helm override behavior for the published sizing files.
func TestSizingExamples(t *testing.T) {
	read := func(path string) map[string]interface{} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var result map[string]interface{}
		require.NoError(t, yaml.Unmarshal(data, &result))
		return result
	}
	render := func(args ...string) []byte {
		return command(t, "helm", append([]string{"template", "nktp", "../../nktp"}, args...)...)
	}
	smallPath := "../../docs/examples/sizing-small.values.yaml"
	largePath := "../../docs/examples/sizing-xlarge.values.yaml"
	small, large := read(smallPath), read(largePath)
	require.Equal(t, sizingPaths(large, ""), sizingPaths(small, ""))
	require.Len(t, small, 8, "seven Collectors and KSM")
	baseline := render()
	require.Equal(t, string(baseline), string(render("-f", largePath)))
	baselineSpecs := map[string]interface{}{}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(baseline), 4096)
	for {
		var resource unstructured.Unstructured
		err := decoder.Decode(&resource)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if resource.GetKind() == "OpenTelemetryCollector" {
			baselineSpecs[resource.GetName()] = resource.Object["spec"]
		}
	}
	for _, path := range []string{smallPath, largePath} {
		t.Run(path, func(t *testing.T) {
			overlay := read(path)
			decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(render("-f", path)), 4096)
			collectors, allocators, ksm := 0, 0, 0
			for {
				var resource unstructured.Unstructured
				err := decoder.Decode(&resource)
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if resource.GetKind() == "OpenTelemetryCollector" {
					name := strings.ReplaceAll(strings.TrimPrefix(resource.GetName(), "nktp-"), "-", "_")
					spec := resource.Object["spec"].(map[string]interface{})
					baselineSpec := baselineSpecs[resource.GetName()].(map[string]interface{})
					if name == "nodeless_metrics" {
						require.NotContains(t, overlay, name)
						require.Equal(t, baselineSpec, spec)
						continue
					}
					collectors++
					component := overlay[name].(map[string]interface{})
					require.NotContains(t, component, "sidecar")
					require.Equal(t, baselineSpec["initContainers"], spec["initContainers"])
					assertSizingSubset(t, component["kubernetes"], spec)
					kubernetes := component["kubernetes"].(map[string]interface{})
					if _, ok := kubernetes["targetAllocator"]; ok {
						allocators++
					}
					assertSizingSubset(t, component["otelcol"].(map[string]interface{})["config"], spec["config"])
				}
				if (resource.GetKind() == "Deployment" || resource.GetKind() == "DaemonSet") && strings.HasPrefix(resource.GetName(), "nktp-ksm-") {
					ksm++
					expected := overlay["kube-state-metrics"].(map[string]interface{})
					if resource.GetKind() == "DaemonSet" {
						expected = expected["daemonset"].(map[string]interface{})
					}
					containers, _, err := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "containers")
					require.NoError(t, err)
					assertSizingSubset(t, expected["kubernetes"].(map[string]interface{})["resources"], containers[0].(map[string]interface{})["resources"])
				}
			}
			require.Equal(t, 7, collectors)
			require.Equal(t, 3, allocators)
			require.Equal(t, 7, ksm)
		})
	}
	overrides := t.TempDir() + "/values.yaml"
	require.NoError(t, os.WriteFile(overrides, []byte(`gateway_logs:
  kubernetes:
    resources:
      requests:
        cpu: 75m
    autoscaler: null
    env:
      - name: GOGC
        value: "75"
  otelcol:
    config:
      exporters:
        otlp_grpc:
          sending_queue:
            queue_size: 12345
`), 0600))
	var gateway unstructured.Unstructured
	require.NoError(t, yaml.Unmarshal(render("-f", smallPath, "-f", overrides, "--show-only", "templates/gateway-logs.yaml"), &gateway.Object))
	cpu, _, err := unstructured.NestedString(gateway.Object, "spec", "resources", "requests", "cpu")
	require.NoError(t, err)
	require.Equal(t, "75m", cpu)
	_, found, err := unstructured.NestedFieldNoCopy(gateway.Object, "spec", "autoscaler")
	require.NoError(t, err)
	require.False(t, found)
	env, _, err := unstructured.NestedSlice(gateway.Object, "spec", "env")
	require.NoError(t, err)
	require.Equal(t, []interface{}{map[string]interface{}{"name": "GOGC", "value": "75"}}, env)
	queue, _, err := unstructured.NestedFieldNoCopy(gateway.Object, "spec", "config", "exporters", "otlp_grpc", "sending_queue", "queue_size")
	require.NoError(t, err)
	require.EqualValues(t, 12345, queue)
}

func sizingPaths(values map[string]interface{}, prefix string) map[string]bool {
	paths := map[string]bool{}
	for key, value := range values {
		path := prefix + "/" + key
		if child, ok := value.(map[string]interface{}); ok {
			for leaf := range sizingPaths(child, path) {
				paths[leaf] = true
			}
		} else {
			paths[path] = true
		}
	}
	return paths
}

// Maps allow unrelated chart settings. Lists must retain every declared entry.
func assertSizingSubset(t *testing.T, expected, actual interface{}) {
	t.Helper()
	switch want := expected.(type) {
	case map[string]interface{}:
		got, ok := actual.(map[string]interface{})
		require.True(t, ok, "expected map, got %T", actual)
		for key, value := range want {
			assertSizingSubset(t, value, got[key])
		}
	case []interface{}:
		got, ok := actual.([]interface{})
		require.True(t, ok)
		// JSON normalization removes decoder-specific numeric types.
		wantJSON, err := json.Marshal(want)
		require.NoError(t, err)
		gotJSON, err := json.Marshal(got)
		require.NoError(t, err)
		var normalizedWant, normalizedGot []interface{}
		require.NoError(t, json.Unmarshal(wantJSON, &normalizedWant))
		require.NoError(t, json.Unmarshal(gotJSON, &normalizedGot))
		for _, value := range normalizedWant {
			require.Contains(t, normalizedGot, value)
		}
	default:
		require.EqualValues(t, expected, actual)
	}
}
