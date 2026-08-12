package observe

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestRegistryCounterHistogramAndPrometheus(t *testing.T) {
	t.Parallel()
	registry, _ := New(10)
	if err := registry.Register(Definition{Name: "gofer_runs_total", Help: "Runs", Kind: Counter, Labels: []string{"status"}, AllowedValues: map[string][]string{"status": {"success", "error"}}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Definition{Name: "gofer_duration_seconds", Help: "Duration", Kind: Histogram, Buckets: []float64{1, 5}, Labels: []string{"kind"}, AllowedValues: map[string][]string{"kind": {"run"}}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add("gofer_runs_total", map[string]string{"status": "success"}, 2); err != nil {
		t.Fatal(err)
	}
	if err := registry.Observe("gofer_duration_seconds", map[string]string{"kind": "run"}, 3); err != nil {
		t.Fatal(err)
	}
	samples := registry.Snapshot()
	if len(samples) != 2 || samples[1].Value != 2 || samples[0].Count != 1 || samples[0].Buckets[0].Count != 0 || samples[0].Buckets[1].Count != 1 {
		t.Fatalf("samples=%#v", samples)
	}
	var output bytes.Buffer
	if err := registry.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "gofer_runs_total{status=\"success\"} 2") || !strings.Contains(text, "gofer_duration_seconds_bucket{kind=\"run\",le=\"5\"} 1") {
		t.Fatalf("output=%s", text)
	}
}

func TestRegistryValidationAndCapacity(t *testing.T) {
	t.Parallel()
	if _, err := New(0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New=%v", err)
	}
	registry, _ := New(1)
	invalid := []Definition{{}, {Name: "bad-name", Help: "x", Kind: Counter}, {Name: "x", Help: "", Kind: Counter}, {Name: "x", Help: "x", Kind: "bad"}, {Name: "x", Help: "x", Kind: Counter, Labels: []string{"a"}}, {Name: "x", Help: "x", Kind: Histogram}}
	for _, definition := range invalid {
		if err := registry.Register(definition); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Register(%#v)=%v", definition, err)
		}
	}
	definition := Definition{Name: "x", Help: "x", Kind: Counter, Labels: []string{"kind"}, AllowedValues: map[string][]string{"kind": {"a", "b"}}}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate=%v", err)
	}
	if err := registry.Add("x", map[string]string{"kind": "bad"}, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("label=%v", err)
	}
	if err := registry.Add("x", map[string]string{"kind": "a"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add("x", map[string]string{"kind": "b"}, 1); err == nil {
		t.Fatal("capacity succeeded")
	}
	if err := registry.Add("x", map[string]string{"kind": "a"}, -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative=%v", err)
	}
	if err := registry.Observe("x", map[string]string{"kind": "a"}, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong kind=%v", err)
	}
}

func TestRegistryConcurrentAddsAndIsolation(t *testing.T) {
	t.Parallel()
	registry, _ := New(2)
	_ = registry.Register(Definition{Name: "x", Help: "x", Kind: Counter})
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() { defer wait.Done(); _ = registry.Add("x", nil, 1) }()
	}
	wait.Wait()
	samples := registry.Snapshot()
	if len(samples) != 1 || samples[0].Value != 100 {
		t.Fatalf("samples=%#v", samples)
	}
	samples[0].Labels["x"] = "bad"
	if len(registry.Snapshot()[0].Labels) != 0 {
		t.Fatal("snapshot shares labels")
	}
}
