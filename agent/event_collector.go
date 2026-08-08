package agent

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
)

type EventCollector struct {
	source EventSource
	limit  int
}

func NewEventCollector(source EventSource) *EventCollector {
	return &EventCollector{source: source, limit: 50}
}

func (c *EventCollector) Collect(ctx context.Context, namespace string) ([]domain.KubernetesEvent, error) {
	events, err := c.source.ListEvents(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return BuildEnvironmentEvents(events, c.limit), nil
}

func BuildEnvironmentEvents(events []KubernetesEvent, limit int) []domain.KubernetesEvent {
	if limit <= 0 {
		limit = 50
	}
	items := make([]domain.KubernetesEvent, 0, len(events))
	for _, event := range events {
		normalized := normalizeKubernetesEvent(event)
		if strings.TrimSpace(normalized.Message) == "" && strings.TrimSpace(normalized.Reason) == "" {
			continue
		}
		items = append(items, normalized)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return eventTimestamp(items[i]).After(eventTimestamp(items[j]))
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func normalizeKubernetesEvent(event KubernetesEvent) domain.KubernetesEvent {
	firstSeen := parseKubernetesTime(firstNonEmpty(event.FirstTimestamp, event.EventTime))
	lastSeen := parseKubernetesTime(firstNonEmpty(event.LastTimestamp, event.EventTime, event.FirstTimestamp))
	return domain.KubernetesEvent{
		UID:          firstNonEmpty(event.Metadata.UID, event.Metadata.Name),
		Namespace:    event.Metadata.Namespace,
		Type:         event.Type,
		Reason:       event.Reason,
		Message:      event.Message,
		InvolvedKind: event.InvolvedObject.Kind,
		InvolvedName: event.InvolvedObject.Name,
		Count:        event.Count,
		FirstSeen:    firstSeen,
		LastSeen:     lastSeen,
	}
}

func parseKubernetesTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC()
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

func eventTimestamp(event domain.KubernetesEvent) time.Time {
	if !event.LastSeen.IsZero() {
		return event.LastSeen
	}
	return event.FirstSeen
}
