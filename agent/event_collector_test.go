package agent

import (
	"context"
	"testing"
)

func TestEventCollectorNormalizesAndSortsKubernetesEvents(t *testing.T) {
	events := BuildEnvironmentEvents([]KubernetesEvent{
		{
			Metadata:       EventMetadata{Name: "old", Namespace: "envpilot-pr-kan-404"},
			Type:           "Normal",
			Reason:         "Pulled",
			Message:        "Successfully pulled image",
			InvolvedObject: InvolvedObject{Kind: "Pod", Name: "cms-api-old"},
			Count:          1,
			LastTimestamp:  "2026-05-01T09:00:00Z",
		},
		{
			Metadata:       EventMetadata{UID: "new", Namespace: "envpilot-pr-kan-404"},
			Type:           "Warning",
			Reason:         "FailedScheduling",
			Message:        "0/3 nodes are available",
			InvolvedObject: InvolvedObject{Kind: "Pod", Name: "cms-api-new"},
			Count:          3,
			LastTimestamp:  "2026-05-01T10:00:00Z",
		},
	}, 10)

	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].UID != "new" {
		t.Fatalf("expected newest event first, got %#v", events[0])
	}
	if events[0].InvolvedKind != "Pod" || events[0].InvolvedName != "cms-api-new" {
		t.Fatalf("involved object = %#v", events[0])
	}
	if events[0].LastSeen.IsZero() {
		t.Fatal("lastSeen was not parsed")
	}
}

func TestEventCollectorUsesKubernetesSource(t *testing.T) {
	source := &fakeEventSource{
		events: []KubernetesEvent{
			{
				Metadata:      EventMetadata{Name: "event-1", Namespace: "envpilot-pr-kan-404"},
				Type:          "Warning",
				Reason:        "BackOff",
				Message:       "Back-off restarting failed container",
				LastTimestamp: "2026-05-01T10:00:00Z",
			},
		},
	}
	collector := NewEventCollector(source)

	events, err := collector.Collect(context.Background(), "envpilot-pr-kan-404")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if source.namespace != "envpilot-pr-kan-404" {
		t.Fatalf("namespace = %q", source.namespace)
	}
	if len(events) != 1 || events[0].Reason != "BackOff" {
		t.Fatalf("events = %#v", events)
	}
}

func TestEventCollectorLimitsEvents(t *testing.T) {
	raw := make([]KubernetesEvent, 0, 55)
	for i := 0; i < 55; i++ {
		raw = append(raw, KubernetesEvent{
			Metadata: EventMetadata{Name: "event"},
			Reason:   "Scheduled",
			Message:  "pod scheduled",
		})
	}
	events := BuildEnvironmentEvents(raw, 50)
	if len(events) != 50 {
		t.Fatalf("events = %d", len(events))
	}
}
