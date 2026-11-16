// Command streamprobe connects to /v1/stream, subscribes to topics and prints
// a sample of the messages (driver arrays are summarised).
//
//	go run ./cmd/streamprobe -url wss://dispatch-go.onrender.com/v1/stream -topics drivers,ops -n 12
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/v1/stream", "stream URL")
	topics := flag.String("topics", "drivers,ops", "comma-separated topics")
	n := flag.Int("n", 10, "messages to print")
	origin := flag.String("origin", "", "Origin header (e.g. https://hexago-web.onrender.com)")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout (covers Render cold starts)")
	flag.Parse()

	h := http.Header{}
	if *origin != "" {
		h.Set("Origin", *origin)
	}
	d := websocket.Dialer{HandshakeTimeout: *timeout}
	conn, resp, err := d.Dial(*url, h)
	if err != nil {
		log.Fatalf("dial %s: %v (%v)", *url, err, resp)
	}
	defer conn.Close()
	sub := map[string]any{"action": "subscribe", "topics": strings.Split(*topics, ",")}
	if err := conn.WriteJSON(sub); err != nil {
		log.Fatal(err)
	}
	b, _ := json.Marshal(sub)
	fmt.Printf("> %s\n", b)

	deadline := time.Now().Add(*timeout)
	for i := 0; i < *n; i++ {
		_ = conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Fatalf("read: %v", err)
		}
		fmt.Printf("< [%s] %s\n", time.Now().UTC().Format("15:04:05"), summarise(msg))
	}
}

func summarise(msg []byte) string {
	var env struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg, &env); err != nil || env.Topic != "drivers" {
		return string(msg)
	}
	var ds []map[string]any
	_ = json.Unmarshal(env.Data, &ds)
	counts := map[string]int{}
	av := 0
	for _, d := range ds {
		counts[fmt.Sprint(d["status"])]++
		if d["tier"] == "av" {
			av++
		}
	}
	first := ""
	if len(ds) > 0 {
		f, _ := json.Marshal(ds[0])
		first = string(f)
	}
	return fmt.Sprintf(`{"topic":"drivers","data":[%s, ... %d drivers]} statuses=%v avs=%d`, first, len(ds), counts, av)
}
