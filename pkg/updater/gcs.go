/*
Copyright 2020 The TestGrid Authors.

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

package updater

import (
	"fmt"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/GoogleCloudPlatform/testgrid/internal/result"
	"github.com/GoogleCloudPlatform/testgrid/metadata"
	"github.com/GoogleCloudPlatform/testgrid/metadata/junit"
	statepb "github.com/GoogleCloudPlatform/testgrid/pb/state"
	statuspb "github.com/GoogleCloudPlatform/testgrid/pb/test_status"
	"github.com/GoogleCloudPlatform/testgrid/util/gcs"
)

// gcsResult holds all the downloaded information for a build of a job.
//
// The suite results become rows and the job metadata is added to the column.
type gcsResult struct {
	podInfo  gcs.PodInfo
	started  gcs.Started
	finished gcs.Finished
	suites   []gcs.SuitesMeta
	job      string
	build    string
}

const maxDuplicates = 20

var overflowCell = Cell{
	Result:  statuspb.TestStatus_FAIL,
	Icon:    "...",
	Message: "Too many duplicately named rows",
}

func propertyMap(r *junit.Result) map[string][]string {
	out := map[string][]string{}
	if r.Properties == nil {
		return out
	}
	for _, p := range r.Properties.PropertyList {
		out[p.Name] = append(out[p.Name], p.Value)
	}
	return out
}

func Means(properties map[string][]string) map[string]float64 {
	out := make(map[string]float64, len(properties))
	for name, values := range properties {
		var sum float64
		var n int
		for _, str := range values {
			v, err := strconv.ParseFloat(str, 64)
			if err != nil {
				continue
			}
			sum += v
			n++
		}
		if n == 0 {
			continue
		}
		out[name] = sum / float64(n)
	}
	return out
}

func first(properties map[string][]string) map[string]string {
	out := make(map[string]string, len(properties))
	for k, v := range properties {
		if len(v) == 0 {
			continue
		}
		out[k] = v[0]
	}
	return out
}

const (
	overallRow = "Overall"
	podInfoRow = "Pod"
)

func MergeCells(cells ...Cell) Cell {
	var out Cell
	if len(cells) == 0 {
		panic("empty cells")
	}
	out = cells[0]

	if len(cells) == 1 {
		return out
	}

	var pass int
	var passMsg string
	var fail int
	var failMsg string

	// determine the status and potential messages
	// gather all metrics
	means := map[string][]float64{}

	current := out.Result

	for _, c := range cells {
		if result.GTE(c.Result, current) {
			current = c.Result
		}
		switch {
		case result.Passing(c.Result):
			pass++
			if passMsg == "" && c.Message != "" {
				passMsg = c.Message
			}
		case result.Failing(c.Result):
			fail++
			if failMsg == "" && c.Message != "" {
				failMsg = c.Message
			}
		}

		for metric, mean := range c.Metrics {
			means[metric] = append(means[metric], mean)
		}
	}
	if pass > 0 && fail > 0 {
		out.Result = statuspb.TestStatus_FLAKY
	} else {
		out.Result = current
	}

	// determine the icon
	total := len(cells)
	out.Icon = strconv.Itoa(pass) + "/" + strconv.Itoa(total)

	// compile the message
	var msg string
	if failMsg != "" {
		msg = failMsg
	} else if passMsg != "" {
		msg = passMsg
	}

	if msg != "" {
		msg = ": " + msg
	}
	out.Message = out.Icon + " runs passed" + msg

	// merge metrics
	if len(means) > 0 {
		out.Metrics = make(map[string]float64, len(means))
		for metric, means := range means {
			var sum float64
			for _, m := range means {
				sum += m
			}
			out.Metrics[metric] = sum / float64(len(means))
		}
	}
	return out
}

func SplitCells(originalName string, cells ...Cell) map[string]Cell {
	n := len(cells)
	if n == 0 {
		return nil
	}
	if n > maxDuplicates {
		n = maxDuplicates
	}
	out := make(map[string]Cell, n)
	for idx, c := range cells {
		// Ensure each name is unique
		// If we have multiple results with the same name foo
		// then append " [n]" to the name so we wind up with:
		//   foo
		//   foo [1]
		//   foo [2]
		//   etc
		name := originalName
		switch idx {
		case 0:
			// nothing
		case maxDuplicates:
			name = name + " [overflow]"
			out[name] = overflowCell
			return out
		default:
			name = name + " [" + strconv.Itoa(idx) + "]"
		}
		out[name] = c
	}
	return out
}

// convertResult returns an inflatedColumn representation of the GCS result.
func convertResult(log logrus.FieldLogger, nameCfg nameConfig, id string, headers []string, metricKey string, result gcsResult, opt groupOptions) (*inflatedColumn, error) {
	cells := map[string][]Cell{}
	var cellID string
	if nameCfg.multiJob {
		cellID = result.job + "/" + id
	}

	meta := result.finished.Metadata.Strings()
	version := metadata.Version(result.started.Started, result.finished.Finished)

	// Append each result into the column
	for _, suite := range result.suites {
		for _, r := range flattenResults(suite.Suites.Suites...) {
			if r.Skipped != nil && *r.Skipped == "" {
				continue
			}
			c := Cell{CellID: cellID}
			if elapsed := r.Time; elapsed > 0 {
				c.Metrics = setElapsed(c.Metrics, elapsed)
			}

			props := propertyMap(&r)
			for metric, mean := range Means(props) {
				if c.Metrics == nil {
					c.Metrics = map[string]float64{}
				}
				c.Metrics[metric] = mean
			}

			const max = 140
			if msg := r.Message(max); msg != "" {
				c.Message = msg
			}

			switch {
			case r.Failure != nil:
				c.Result = statuspb.TestStatus_FAIL
				if c.Message != "" {
					c.Icon = "F"
				}
			case r.Skipped != nil:
				c.Result = statuspb.TestStatus_PASS_WITH_SKIPS
				c.Icon = "S"
			default:
				c.Result = statuspb.TestStatus_PASS
			}

			if f, ok := c.Metrics[metricKey]; ok {
				c.Icon = strconv.FormatFloat(f, 'g', 4, 64)
			}

			name := nameCfg.render(result.job, r.Name, first(props), suite.Metadata, meta)
			cells[name] = append(cells[name], c)
		}
	}

	overall := overallCell(result)
	if overall.Result == statuspb.TestStatus_FAIL && overall.Message == "" { // Ensure failing build has a failing cell and/or overall message
		var found bool
		for _, namedCells := range cells {
			for _, c := range namedCells {
				if c.Result == statuspb.TestStatus_FAIL {
					found = true // Failing test, huzzah!
					break
				}
			}
			if found {
				break
			}
		}
		if !found { // Nope, add the F icon and an explanatory Message
			overall.Icon = "F"
			overall.Message = "Build failed outside of test results"
		}
	}

	injectedCells := map[string]Cell{
		overallRow: overall,
	}

	if opt.analyzeProwJob {
		if pic := podInfoCell(result.podInfo); pic.Message != gcs.MissingPodInfo || overall.Result != statuspb.TestStatus_RUNNING {
			injectedCells[podInfoRow] = pic
		}
	}

	for name, c := range injectedCells {
		if nameCfg.multiJob {
			c.CellID = cellID
			jobName := result.job + "." + name
			cells[jobName] = append([]Cell{c}, cells[jobName]...)
		}
		cells[name] = append([]Cell{c}, cells[name]...)
	}

	out := inflatedColumn{
		Column: &statepb.Column{
			Build:   id,
			Started: float64(result.started.Timestamp * 1000),
		},
		Cells: map[string]Cell{},
	}

	for name, cells := range cells {
		switch {
		case opt.merge:
			out.Cells[name] = MergeCells(cells...)
		default:
			for n, c := range SplitCells(name, cells...) {
				out.Cells[n] = c
			}
		}
	}

	for _, h := range headers {
		val, ok := meta[h]
		if !ok && h == "Commit" && version != metadata.Missing {
			val = version
		} else if !ok && overall.Result != statuspb.TestStatus_RUNNING {
			val = "missing"
		}
		out.Column.Extra = append(out.Column.Extra, val)
	}

	return &out, nil
}

func podInfoCell(podInfo gcs.PodInfo) Cell {
	pass, msg := podInfo.Summarize()
	var status statuspb.TestStatus
	var icon string
	if pass {
		status = statuspb.TestStatus_PASS
	} else {
		status = statuspb.TestStatus_FAIL
	}

	switch {
	case msg == gcs.NoPodUtils:
		icon = "E"
	case msg == gcs.MissingPodInfo:
		icon = "!"
	case !pass:
		icon = "F"
	}

	return Cell{
		Message: msg,
		Icon:    icon,
		Result:  status,
	}
}

// overallCell generates the overall cell for this GCS result.
func overallCell(result gcsResult) Cell {
	var c Cell
	var finished int64
	if result.finished.Timestamp != nil {
		finished = *result.finished.Timestamp
	}
	switch {
	case finished > 0: // completed result
		var passed bool
		res := result.finished.Result
		switch {
		case result.finished.Passed == nil:
			if res != "" {
				passed = res == "SUCCESS"
				c.Icon = "E"
				c.Message = fmt.Sprintf(`finished.json missing "passed": %t`, passed)
			}
		case result.finished.Passed != nil:
			passed = *result.finished.Passed
		}

		if passed {
			c.Result = statuspb.TestStatus_PASS
		} else {
			c.Result = statuspb.TestStatus_FAIL
		}
		c.Metrics = setElapsed(nil, float64(finished-result.started.Timestamp))
	case time.Now().Add(-24*time.Hour).Unix() > result.started.Timestamp:
		c.Result = statuspb.TestStatus_FAIL
		c.Message = "Build did not complete within 24 hours"
		c.Icon = "T"
	default:
		c.Result = statuspb.TestStatus_RUNNING
		c.Message = "Build still running..."
		c.Icon = "R"
	}
	return c
}

const ElapsedKey = "test-duration-minutes"

// setElapsed inserts the seconds-elapsed metric.
func setElapsed(metrics map[string]float64, seconds float64) map[string]float64 {
	if metrics == nil {
		metrics = map[string]float64{}
	}
	metrics[ElapsedKey] = seconds / 60
	return metrics
}

// flattenResults returns the DFS of all junit results in all suites.
func flattenResults(suites ...junit.Suite) []junit.Result {
	var results []junit.Result
	for _, suite := range suites {
		for _, innerSuite := range suite.Suites {
			innerSuite.Name = dotName(suite.Name, innerSuite.Name)
			results = append(results, flattenResults(innerSuite)...)
		}
		for _, r := range suite.Results {
			r.Name = dotName(suite.Name, r.Name)
			results = append(results, r)
		}
	}
	return results
}

// dotName returns left.right or left or right
func dotName(left, right string) string {
	if left != "" && right != "" {
		return left + "." + right
	}
	if right == "" {
		return left
	}
	return right
}
