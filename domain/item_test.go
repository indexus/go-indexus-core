package domain

import "testing"

func TestParseContentRoundTrip(t *testing.T) {
	item := &Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1, 2.5, 3}}
	got, ok := ParseContent(item.Content())
	if !ok {
		t.Fatal("ParseContent rejected Content()")
	}
	if got.Collection != item.Collection || got.Location != item.Location || got.Id != item.Id {
		t.Fatalf("identity: got %+v", got)
	}
	if len(got.Metrics) != 3 || got.Metrics[0] != 1 || got.Metrics[1] != 2.5 || got.Metrics[2] != 3 {
		t.Fatalf("metrics: got %v", got.Metrics)
	}
}

func TestParseContentItemPrefix(t *testing.T) {
	item := &Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{4}}
	got, ok := ParseContent("item|" + item.Content())
	if !ok || got.Id != "x" || len(got.Metrics) != 1 || got.Metrics[0] != 4 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseContentTombstone(t *testing.T) {
	item := &Item{Collection: "demo", Location: "aa", Id: "x", Tombstone: true, Gen: 7}
	got, ok := ParseContent(item.Content())
	if !ok || !got.Tombstone || got.Gen != 7 {
		t.Fatalf("got %+v", got)
	}
}
