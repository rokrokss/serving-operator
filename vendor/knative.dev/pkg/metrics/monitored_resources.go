/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"go.opencensus.io/tag"
	"knative.dev/pkg/metrics/metricskey"
)

type Global struct{}

func (g *Global) MonitoredResource() (resType string, labels map[string]string) {
	return "global", nil
}

func getTagsMap(tags []tag.Tag) map[string]string {
	tagsMap := map[string]string{}
	for _, t := range tags {
		tagsMap[t.Key.Name()] = t.Value
	}
	return tagsMap
}

func valueOrUnknown(key string, tagsMap map[string]string) string {
	if value, ok := tagsMap[key]; ok {
		return value
	}
	return metricskey.ValueUnknown
}
