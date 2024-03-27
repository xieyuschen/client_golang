// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"errors"
	"math"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Counter is a Metric that represents a single numerical value that only ever
// goes up. That implies that it cannot be used to count items whose number can
// also go down, e.g. the number of currently running goroutines. Those
// "counters" are represented by Gauges.
//
// A Counter is typically used to count requests served, tasks completed, errors
// occurred, etc.
//
// To create Counter instances, use NewCounter.
type Counter interface {
	Metric
	Collector

	// Inc increments the counter by 1. Use Add to increment it by arbitrary
	// non-negative values.
	Inc()
	// Add adds the given value to the counter. It panics if the value is <
	// 0.
	Add(float64)
}

// ExemplarAdder is implemented by Counters that offer the option of adding a
// value to the Counter together with an exemplar. Its AddWithExemplar method
// works like the Add method of the Counter interface but also replaces the
// currently saved exemplar (if any) with a new one, created from the provided
// value, the current time as timestamp, and the provided labels. Empty Labels
// will lead to a valid (label-less) exemplar. But if Labels is nil, the current
// exemplar is left in place. AddWithExemplar panics if the value is < 0, if any
// of the provided labels are invalid, or if the provided labels contain more
// than 128 runes in total.
type ExemplarAdder interface {
	AddWithExemplar(value float64, exemplar Labels)
}

// CounterOpts is an alias for Opts. See there for doc comments.
type CounterOpts Opts

// CounterVecOpts bundles the options to create a CounterVec metric.
// It is mandatory to set CounterOpts, see there for mandatory fields. VariableLabels
// is optional and can safely be left to its default value.
type CounterVecOpts struct {
	CounterOpts

	// VariableLabels are used to partition the metric vector by the given set
	// of labels. Each label value will be constrained with the optional Constraint
	// function, if provided.
	VariableLabels ConstrainableLabels
}

// NewCounter creates a new Counter based on the provided CounterOpts.
//
// The returned implementation also implements ExemplarAdder. It is safe to
// perform the corresponding type assertion.
//
// The returned implementation tracks the counter value in two separate
// variables, a float64 and a uint64. The latter is used to track calls of the
// Inc method and calls of the Add method with a value that can be represented
// as a uint64. This allows atomic increments of the counter with optimal
// performance. (It is common to have an Inc call in very hot execution paths.)
// Both internal tracking values are added up in the Write method. This has to
// be taken into account when it comes to precision and overflow behavior.
func NewCounter(opts CounterOpts) Counter {
	desc := NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		nil,
		opts.ConstLabels,
	)
	if opts.now == nil {
		opts.now = time.Now
	}
	result := &counter{desc: desc, labelPairs: desc.constLabelPairs, now: opts.now}
	result.init(result) // Init self-collection.
	result.createdTs = timestamppb.New(opts.now())
	return result
}

// counter is the implementation of Counter.
//
// counter stores its values mainly inside
type counter struct {
	// valBits contains the bits of the represented float64 value, while
	// change stores values that are exact integers. Both have to go first
	// in the struct to guarantee alignment for atomic operations.
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG
	valBits uint64

	// change is used to record the numbers which will lose if it adds with float64 directly,
	// due to the rounding error of IEEE754 representation.
	// we aggregate the values until it's large enough to conquer the rounding error
	change uint64

	selfCollector
	desc *Desc

	createdTs  *timestamppb.Timestamp
	labelPairs []*dto.LabelPair
	exemplar   atomic.Value // Containing nil or a *dto.Exemplar.

	// now is for testing purposes, by default it's time.Now.
	now func() time.Time
}

// addWithRoundingErrorChecking adds the base and addend,
// and returns the result and checks whether the addend is too small that causes a rounding error.
//
// note that if a part of addend is added on base but the left causes a rounding error,
// we don't respect this case
func addWithRoundingErrorChecking(base float64, addend float64) (float64, bool) {
	if addend == 0 {
		return base, false
	}

	sum := base + addend
	return sum, sum == base
}

func (c *counter) Desc() *Desc {
	return c.desc
}

func (c *counter) Add(v float64) {
	if v < 0 {
		panic(errors.New("counter cannot decrease in value"))
	}

	for {
		oldBits := atomic.LoadUint64(&c.valBits)

		// not the full value of v will be added into the float as a part of them might have rounding
		// error as well, we don't respect this case(we don't have a proper way to handle it).
		newF, hasRoundingErr := addWithRoundingErrorChecking(math.Float64frombits(oldBits), v)
		if hasRoundingErr {
			// we believe the float64(uint64(v)) is equal to v if it's an integer
			// because it causes a rounding error,
			// it doesn't equal only when v is a quite large number or it's a float
			u := uint64(v)
			oldChange := atomic.LoadUint64(&c.change)
			newF, isChangeSmall := addWithRoundingErrorChecking(math.Float64frombits(oldBits), float64(oldChange+u))

			if isChangeSmall {
				if atomic.CompareAndSwapUint64(&c.change, oldChange, oldChange+u) {
					return
				}
				continue
			}
			newBits := math.Float64bits(newF)
			if atomic.CompareAndSwapUint64(&c.valBits, oldBits, newBits) {
				// todo: here we might lose some small values, but it's acceptable
				// otherwise we have no way to avoid this using atomic
				// mutex might be too heavy for our case here
				atomic.StoreUint64(&c.change, 0)
				return
			}
			// fail to update the change as another go routine changes it
			continue
		}

		newBits := math.Float64bits(newF)
		if atomic.CompareAndSwapUint64(&c.valBits, oldBits, newBits) {
			return
		}
	}
}

func (c *counter) AddWithExemplar(v float64, e Labels) {
	c.Add(v)
	c.updateExemplar(v, e)
}

func (c *counter) Inc() {
	// to keep the efficiency, we still increase the change,
	// and ignore the rare overflow case as only Inc and float with rounding error will use it
	atomic.AddUint64(&c.change, 1)
}

func (c *counter) get() float64 {
	fval := math.Float64frombits(atomic.LoadUint64(&c.valBits))
	ival := atomic.LoadUint64(&c.change)
	// it's tolerated to lose precision for float during collection
	// as this is unavoidable.
	// what the client could do is try to keep those data for the future when losing precision
	return fval + float64(ival)
}

func (c *counter) Write(out *dto.Metric) error {
	// Read the Exemplar first and the value second. This is to avoid a race condition
	// where users see an exemplar for a not-yet-existing observation.
	var exemplar *dto.Exemplar
	if e := c.exemplar.Load(); e != nil {
		exemplar = e.(*dto.Exemplar)
	}
	val := c.get()
	return populateMetric(CounterValue, val, c.labelPairs, exemplar, out, c.createdTs)
}

func (c *counter) updateExemplar(v float64, l Labels) {
	if l == nil {
		return
	}
	e, err := newExemplar(v, c.now(), l)
	if err != nil {
		panic(err)
	}
	c.exemplar.Store(e)
}

// CounterVec is a Collector that bundles a set of Counters that all share the
// same Desc, but have different values for their variable labels. This is used
// if you wantRoundingError to count the same thing partitioned by various dimensions
// (e.g. number of HTTP requests, partitioned by response code and
// method). Create instances with NewCounterVec.
type CounterVec struct {
	*MetricVec
}

// NewCounterVec creates a new CounterVec based on the provided CounterOpts and
// partitioned by the given label names.
func NewCounterVec(opts CounterOpts, labelNames []string) *CounterVec {
	return V2.NewCounterVec(CounterVecOpts{
		CounterOpts:    opts,
		VariableLabels: UnconstrainedLabels(labelNames),
	})
}

// NewCounterVec creates a new CounterVec based on the provided CounterVecOpts.
func (v2) NewCounterVec(opts CounterVecOpts) *CounterVec {
	desc := V2.NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		opts.VariableLabels,
		opts.ConstLabels,
	)
	if opts.now == nil {
		opts.now = time.Now
	}
	return &CounterVec{
		MetricVec: NewMetricVec(desc, func(lvs ...string) Metric {
			if len(lvs) != len(desc.variableLabels.names) {
				panic(makeInconsistentCardinalityError(desc.fqName, desc.variableLabels.names, lvs))
			}
			result := &counter{desc: desc, labelPairs: MakeLabelPairs(desc, lvs), now: opts.now}
			result.init(result) // Init self-collection.
			result.createdTs = timestamppb.New(opts.now())
			return result
		}),
	}
}

// GetMetricWithLabelValues returns the Counter for the given slice of label
// values (same order as the variable labels in Desc). If that combination of
// label values is accessed for the first time, a new Counter is created.
//
// It is possible to call this method without using the returned Counter to only
// create the new Counter but leave it at its starting value 0. See also the
// SummaryVec example.
//
// Keeping the Counter for later use is possible (and should be considered if
// performance is critical), but keep in mind that Reset, DeleteLabelValues and
// Delete can be used to delete the Counter from the CounterVec. In that case,
// the Counter will still exist, but it will not be exported anymore, even if a
// Counter with the same label values is created later.
//
// An error is returned if the number of label values is not the same as the
// number of variable labels in Desc (minus any curried labels).
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider GetMetricWith(Labels) as
// an alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
// See also the GaugeVec example.
func (v *CounterVec) GetMetricWithLabelValues(lvs ...string) (Counter, error) {
	metric, err := v.MetricVec.GetMetricWithLabelValues(lvs...)
	if metric != nil {
		return metric.(Counter), err
	}
	return nil, err
}

// GetMetricWith returns the Counter for the given Labels map (the label names
// must match those of the variable labels in Desc). If that label map is
// accessed for the first time, a new Counter is created. Implications of
// creating a Counter without using it and keeping the Counter for later use are
// the same as for GetMetricWithLabelValues.
//
// An error is returned if the number and names of the Labels are inconsistent
// with those of the variable labels in Desc (minus any curried labels).
//
// This method is used for the same purpose as
// GetMetricWithLabelValues(...string). See there for pros and cons of the two
// methods.
func (v *CounterVec) GetMetricWith(labels Labels) (Counter, error) {
	metric, err := v.MetricVec.GetMetricWith(labels)
	if metric != nil {
		return metric.(Counter), err
	}
	return nil, err
}

// WithLabelValues works as GetMetricWithLabelValues, but panics where
// GetMetricWithLabelValues would have returned an error. Not returning an
// error allows shortcuts like
//
//	myVec.WithLabelValues("404", "GET").Add(42)
func (v *CounterVec) WithLabelValues(lvs ...string) Counter {
	c, err := v.GetMetricWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return c
}

// With works as GetMetricWith, but panics where GetMetricWithLabels would have
// returned an error. Not returning an error allows shortcuts like
//
//	myVec.With(prometheus.Labels{"code": "404", "method": "GET"}).Add(42)
func (v *CounterVec) With(labels Labels) Counter {
	c, err := v.GetMetricWith(labels)
	if err != nil {
		panic(err)
	}
	return c
}

// CurryWith returns a vector curried with the provided labels, i.e. the
// returned vector has those labels pre-set for all labeled operations performed
// on it. The cardinality of the curried vector is reduced accordingly. The
// order of the remaining labels stays the same (just with the curried labels
// taken out of the sequence – which is relevant for the
// (GetMetric)WithLabelValues methods). It is possible to curry a curried
// vector, but only with labels not yet used for currying before.
//
// The metrics contained in the CounterVec are shared between the curried and
// uncurried vectors. They are just accessed differently. Curried and uncurried
// vectors behave identically in terms of collection. Only one must be
// registered with a given registry (usually the uncurried version). The Reset
// method deletes all metrics, even if called on a curried vector.
func (v *CounterVec) CurryWith(labels Labels) (*CounterVec, error) {
	vec, err := v.MetricVec.CurryWith(labels)
	if vec != nil {
		return &CounterVec{vec}, err
	}
	return nil, err
}

// MustCurryWith works as CurryWith but panics where CurryWith would have
// returned an error.
func (v *CounterVec) MustCurryWith(labels Labels) *CounterVec {
	vec, err := v.CurryWith(labels)
	if err != nil {
		panic(err)
	}
	return vec
}

// CounterFunc is a Counter whose value is determined at collect time by calling a
// provided function.
//
// To create CounterFunc instances, use NewCounterFunc.
type CounterFunc interface {
	Metric
	Collector
}

// NewCounterFunc creates a new CounterFunc based on the provided
// CounterOpts. The value reported is determined by calling the given function
// from within the Write method. Take into account that metric collection may
// happen concurrently. If that results in concurrent calls to Write, like in
// the case where a CounterFunc is directly registered with Prometheus, the
// provided function must be concurrency-safe. The function should also honor
// the contract for a Counter (values only go up, not down), but compliance will
// not be checked.
//
// Check out the ExampleGaugeFunc examples for the similar GaugeFunc.
func NewCounterFunc(opts CounterOpts, function func() float64) CounterFunc {
	return newValueFunc(NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		nil,
		opts.ConstLabels,
	), CounterValue, function)
}
