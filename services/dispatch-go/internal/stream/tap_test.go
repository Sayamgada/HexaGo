package stream_test

import (
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

func TestOnPublishObservesEveryPublish(t *testing.T) {
	h := stream.NewHub(stream.HubOptions{})
	var seen []string
	h.OnPublish(func(topic string, data any) { seen = append(seen, topic+":"+data.(string)) })
	h.OnPublish(func(topic string, _ any) { seen = append(seen, "second:"+topic) })
	h.Publish(stream.TopicOps, "a")
	h.PublishLocal(stream.TopicOps, "local frames are not tapped")
	h.Publish("ride:1", "b")
	want := []string{"ops:a", "second:ops", "ride:1:b", "second:ride:1"}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen %v, want %v", seen, want)
		}
	}
}
