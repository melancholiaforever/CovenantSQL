/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package metric

import (
	"sync"

	dto "github.com/prometheus/client_model/go"
	"gitlab.com/thunderdb/ThunderDB/proto"
)

// MetricMap is map from metric name to MetricFamily.
type MetricMap map[string]*dto.MetricFamily

// FilterFunc is a function that knows how to filter a specific node
// that match the metric limits. if node picked return true else false.
type FilterFunc func(key proto.NodeID, value MetricMap) bool

// NodeMetricMap is sync.Map version of map[proto.NodeID]MetricMap.
type NodeMetricMap struct {
	sync.Map // map[proto.NodeID]MetricMap
}

// FilterNode return node id slice make filterFunc return true.
func (nmm *NodeMetricMap) FilterNode(filterFunc FilterFunc) (ret []proto.NodeID) {
	nodePicker := func(key, value interface{}) bool {
		id, ok := key.(proto.NodeID)
		if !ok {
			return true // continue iteration
		}
		metrics, ok := value.(MetricMap)
		if !ok {
			return true // continue iteration
		}
		if filterFunc(id, metrics) {
			ret = append(ret, id)
		}
		return true
	}
	nmm.Range(nodePicker)
	return
}

// GetMetrics returns nodes metrics.
func (nmm *NodeMetricMap) GetMetrics(nodes []proto.NodeID) (metrics map[proto.NodeID]MetricMap) {
	metrics = make(map[proto.NodeID]MetricMap)

	for _, node := range nodes {
		var ok bool
		var rawNodeMetrics interface{}

		if rawNodeMetrics, ok = nmm.Load(node); !ok {
			continue
		}

		var nodeMetrics MetricMap

		if nodeMetrics, ok = rawNodeMetrics.(MetricMap); !ok {
			continue
		}

		metrics[node] = nodeMetrics
	}

	return
}