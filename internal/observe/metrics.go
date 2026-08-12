package observe

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrInvalid identifies a malformed metric definition or observation.
var ErrInvalid = errors.New("invalid metric")
var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Kind identifies a counter or histogram metric.
type Kind string

const (
	// Counter is a monotonically increasing value.
	Counter Kind = "counter"
	// Histogram records observations in fixed cumulative buckets.
	Histogram Kind = "histogram"
)

// Definition predeclares a bounded metric label space.
type Definition struct {
	Name          string
	Help          string
	Kind          Kind
	Labels        []string
	AllowedValues map[string][]string
	Buckets       []float64
}

// Sample is one stable metric series snapshot.
type Sample struct {
	Name    string            `json:"name"`
	Kind    Kind              `json:"kind"`
	Labels  map[string]string `json:"labels"`
	Value   float64           `json:"value"`
	Count   uint64            `json:"count,omitempty"`
	Sum     float64           `json:"sum,omitempty"`
	Buckets []Bucket          `json:"buckets,omitempty"`
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	UpperBound float64 `json:"upper_bound"`
	Count      uint64  `json:"count"`
}

type metric struct {
	definition Definition
	series     map[string]*series
}
type series struct {
	labels  map[string]string
	value   float64
	count   uint64
	sum     float64
	buckets []uint64
}

// Registry owns validated metric definitions and synchronized series.
type Registry struct {
	mu        sync.RWMutex
	metrics   map[string]*metric
	maxSeries int
}

// New constructs a registry with a global series bound.
func New(maxSeries int) (*Registry, error) {
	if maxSeries < 1 || maxSeries > 1_000_000 {
		return nil, ErrInvalid
	}
	return &Registry{metrics: make(map[string]*metric), maxSeries: maxSeries}, nil
}

// Register validates and adds an immutable metric definition.
func (registry *Registry) Register(definition Definition) error {
	if !metricName.MatchString(definition.Name) || strings.TrimSpace(definition.Help) == "" || (definition.Kind != Counter && definition.Kind != Histogram) {
		return ErrInvalid
	}
	definition.Labels = append([]string(nil), definition.Labels...)
	sort.Strings(definition.Labels)
	for index, label := range definition.Labels {
		if !metricName.MatchString(label) || (index > 0 && definition.Labels[index-1] == label) || len(definition.AllowedValues[label]) == 0 {
			return ErrInvalid
		}
		values := append([]string(nil), definition.AllowedValues[label]...)
		sort.Strings(values)
		definition.AllowedValues[label] = values
	}
	if definition.Kind == Histogram {
		if len(definition.Buckets) == 0 {
			return ErrInvalid
		}
		definition.Buckets = append([]float64(nil), definition.Buckets...)
		sort.Float64s(definition.Buckets)
		for index, bucket := range definition.Buckets {
			if bucket <= 0 || (index > 0 && definition.Buckets[index-1] == bucket) {
				return ErrInvalid
			}
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.metrics[definition.Name]; exists {
		return ErrInvalid
	}
	registry.metrics[definition.Name] = &metric{definition: definition, series: make(map[string]*series)}
	return nil
}

// Add increments a counter series by a non-negative value.
func (registry *Registry) Add(name string, labels map[string]string, delta float64) error {
	if delta < 0 {
		return ErrInvalid
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	metric, entry, err := registry.seriesLocked(name, labels)
	if err != nil {
		return err
	}
	if metric.definition.Kind != Counter {
		return ErrInvalid
	}
	entry.value += delta
	return nil
}

// Observe records a non-negative histogram observation.
func (registry *Registry) Observe(name string, labels map[string]string, value float64) error {
	if value < 0 {
		return ErrInvalid
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	metric, entry, err := registry.seriesLocked(name, labels)
	if err != nil {
		return err
	}
	if metric.definition.Kind != Histogram {
		return ErrInvalid
	}
	entry.count++
	entry.sum += value
	for index, bound := range metric.definition.Buckets {
		if value <= bound {
			entry.buckets[index]++
		}
	}
	return nil
}

func (registry *Registry) seriesLocked(name string, labels map[string]string) (*metric, *series, error) {
	metric, exists := registry.metrics[name]
	if !exists {
		return nil, nil, ErrInvalid
	}
	key, cloned, err := labelKey(metric.definition, labels)
	if err != nil {
		return nil, nil, err
	}
	if entry := metric.series[key]; entry != nil {
		return metric, entry, nil
	}
	total := 0
	for _, candidate := range registry.metrics {
		total += len(candidate.series)
	}
	if total >= registry.maxSeries {
		return nil, nil, errors.New("metric series capacity reached")
	}
	entry := &series{labels: cloned, buckets: make([]uint64, len(metric.definition.Buckets))}
	metric.series[key] = entry
	return metric, entry, nil
}

func labelKey(definition Definition, labels map[string]string) (string, map[string]string, error) {
	if len(labels) != len(definition.Labels) {
		return "", nil, ErrInvalid
	}
	cloned := make(map[string]string, len(labels))
	parts := make([]string, len(definition.Labels))
	for index, name := range definition.Labels {
		value, exists := labels[name]
		if !exists || !allowed(value, definition.AllowedValues[name]) {
			return "", nil, ErrInvalid
		}
		cloned[name] = value
		parts[index] = name + "=" + value
	}
	return strings.Join(parts, "\xff"), cloned, nil
}
func allowed(value string, values []string) bool {
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

// Snapshot returns stable metric and label order.
func (registry *Registry) Snapshot() []Sample {
	registry.mu.RLock()
	samples := make([]Sample, 0)
	for _, metric := range registry.metrics {
		for _, entry := range metric.series {
			sample := Sample{Name: metric.definition.Name, Kind: metric.definition.Kind, Labels: cloneLabels(entry.labels), Value: entry.value, Count: entry.count, Sum: entry.sum}
			for index, count := range entry.buckets {
				sample.Buckets = append(sample.Buckets, Bucket{UpperBound: metric.definition.Buckets[index], Count: count})
			}
			samples = append(samples, sample)
		}
	}
	registry.mu.RUnlock()
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Name != samples[j].Name {
			return samples[i].Name < samples[j].Name
		}
		return formatLabels(samples[i].Labels) < formatLabels(samples[j].Labels)
	})
	return samples
}

// WritePrometheus renders the registry in the Prometheus text exposition format.
func (registry *Registry) WritePrometheus(writer io.Writer) error {
	registry.mu.RLock()
	names := make([]string, 0, len(registry.metrics))
	for name := range registry.metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		metric := registry.metrics[name]
		if _, err := fmt.Fprintf(writer, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(metric.definition.Help), name, metric.definition.Kind); err != nil {
			registry.mu.RUnlock()
			return err
		}
		keys := make([]string, 0, len(metric.series))
		for key := range metric.series {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := writeSeries(writer, metric.definition, metric.series[key]); err != nil {
				registry.mu.RUnlock()
				return err
			}
		}
	}
	registry.mu.RUnlock()
	return nil
}

func writeSeries(writer io.Writer, definition Definition, entry *series) error {
	labels := formatLabels(entry.labels)
	if definition.Kind == Counter {
		_, err := fmt.Fprintf(writer, "%s%s %s\n", definition.Name, labels, strconv.FormatFloat(entry.value, 'g', -1, 64))
		return err
	}
	for index, bucket := range definition.Buckets {
		bucketLabels := cloneLabels(entry.labels)
		bucketLabels["le"] = strconv.FormatFloat(bucket, 'g', -1, 64)
		if _, err := fmt.Fprintf(writer, "%s_bucket%s %d\n", definition.Name, formatLabels(bucketLabels), entry.buckets[index]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "%s_count%s %d\n", definition.Name, labels, entry.count); err != nil {
		return err
	}
	_, err := fmt.Fprintf(writer, "%s_sum%s %s\n", definition.Name, labels, strconv.FormatFloat(entry.sum, 'g', -1, 64))
	return err
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for index, name := range names {
		parts[index] = name + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[name]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}
func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for name, value := range labels {
		cloned[name] = value
	}
	return cloned
}
func escapeHelp(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "\n", `\n`)
}
