package drain

import (
	"testing"
	"time"
)

func TestAdd_GroupsRepeatedMessagesIntoOneTemplate(t *testing.T) {
	d := New()

	var lastID int
	for i := 0; i < 1240; i++ {
		id, _ := d.Add("failed to acquire connection: timeout after 500ms")
		lastID = id
	}

	templates := d.Templates()
	if len(templates) != 1 {
		t.Fatalf("want 1 template, got %d: %+v", len(templates), templates)
	}
	if templates[0].Count != 1240 {
		t.Errorf("want count 1240, got %d", templates[0].Count)
	}
	if templates[0].ID != lastID {
		t.Errorf("want stable template id across repeats, got %d vs %d", templates[0].ID, lastID)
	}
}

func TestAdd_WildcardsVaryingNumericToken(t *testing.T) {
	d := New()
	d.Add("connection pool wait 120ms")
	id2, template := d.Add("connection pool wait 340ms")

	if template != "connection pool wait <*>" {
		t.Errorf("want wildcarded numeric token, got %q", template)
	}
	if len(d.Templates()) != 1 {
		t.Errorf("want the two messages merged into one template, got %d", len(d.Templates()))
	}
	if d.Templates()[0].ID != id2 {
		t.Errorf("returned id should match the stored template id")
	}
}

func TestAdd_DifferentTokenCountsNeverMerge(t *testing.T) {
	d := New()
	d.Add("short message")
	d.Add("a somewhat longer message here")

	if len(d.Templates()) != 2 {
		t.Fatalf("want 2 templates for different token counts, got %d", len(d.Templates()))
	}
}

func TestAdd_DissimilarSameLengthMessagesStaySeparate(t *testing.T) {
	d := New()
	d.Add("failed to acquire connection: timeout")
	d.Add("successfully processed order for user")

	templates := d.Templates()
	if len(templates) != 2 {
		t.Fatalf("want 2 distinct templates for dissimilar messages, got %d: %+v", len(templates), templates)
	}
}

func TestAdd_SimilarMessagesBelowThresholdStillMerge(t *testing.T) {
	// 4 tokens, differing in 1 position -> similarity 0.75, above the
	// 0.4 default threshold, so these should merge.
	d := New()
	d.Add("service order call failed")
	id, template := d.Add("service order call succeeded")

	if len(d.Templates()) != 1 {
		t.Fatalf("want messages above simThreshold to merge, got %d templates", len(d.Templates()))
	}
	if template != "service order call <*>" {
		t.Errorf("want wildcarded differing token, got %q", template)
	}
	_ = id
}

func TestTemplates_TracksFirstAndLastSeen(t *testing.T) {
	d := New().(*tree)
	var tick int
	d.now = func() time.Time {
		tick++
		return time.Unix(int64(tick), 0)
	}

	d.Add("retrying request")
	d.Add("retrying request")
	d.Add("retrying request")

	tpl := d.Templates()[0]
	if !tpl.FirstSeen.Before(tpl.LastSeen) {
		t.Errorf("want FirstSeen before LastSeen, got %v vs %v", tpl.FirstSeen, tpl.LastSeen)
	}
	if tpl.Count != 3 {
		t.Errorf("want count 3, got %d", tpl.Count)
	}
}
