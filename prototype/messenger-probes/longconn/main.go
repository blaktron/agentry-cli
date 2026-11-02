// PROTOTYPE, throwaway: measures whether long-lived connections hold through
// the gateway's TCP relay (HAProxy, docker/mcp-l4proxy) under gVisor, for the
// messenger role. Not production code; see the issue for the findings.
//
//	longconn server :8080                 # the upstream, on the egress network
//	longconn client <minutes>             # in the internal network, behind relays
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func logf(f string, a ...any) {
	fmt.Printf("%s "+f+"\n", append([]any{time.Now().UTC().Format("15:04:05")}, a...)...)
}

func main() {
	switch os.Args[1] {
	case "server":
		server(os.Args[2])
	case "client":
		m, _ := strconv.Atoi(os.Args[2])
		client(time.Duration(m) * time.Minute)
	}
}

func server(addr string) {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		mode := r.URL.Query().Get("mode")
		logf("server: ws open mode=%s", mode)
		ctx := r.Context()
		if mode == "ping" {
			go func() {
				for i := 0; ; i++ {
					time.Sleep(40 * time.Second)
					if c.Write(ctx, websocket.MessageText, []byte("tick "+strconv.Itoa(i))) != nil {
						return
					}
				}
			}()
		}
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				logf("server: ws closed mode=%s: %v", mode, err)
				return
			}
			_ = c.Write(ctx, websocket.MessageText, append([]byte("echo "), b...))
		}
	})
	http.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("hold"))
		select {
		case <-time.After(d):
			fmt.Fprintf(w, "held %s\n", d)
		case <-r.Context().Done():
			logf("server: poll hold=%s cancelled", d)
		}
	})
	logf("server listening %s", addr)
	_ = http.ListenAndServe(addr, nil)
}

func client(total time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), total+2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	run := func(name string, f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f(); logf("%s: done", name) }()
	}
	up := "probe-upstream:8080"

	// 1. A WebSocket with traffic every 40 s, as Discord's heartbeat.
	run("ws-ping", func() {
		c, _, err := websocket.Dial(ctx, "ws://"+up+"/ws?mode=ping", nil)
		if err != nil {
			logf("ws-ping: dial: %v", err)
			return
		}
		deadline := time.Now().Add(total)
		n := 0
		for time.Now().Before(deadline) {
			rctx, rc := context.WithTimeout(ctx, 90*time.Second)
			_, b, err := c.Read(rctx)
			rc()
			if err != nil {
				logf("ws-ping: DROPPED after %d ticks: %v", n, err)
				return
			}
			n++
			if n%15 == 0 {
				logf("ws-ping: %d ticks, last %q", n, b)
			}
		}
		logf("ws-ping: HELD %s, %d ticks", total, n)
		c.Close(websocket.StatusNormalClosure, "")
	})

	// 2. WebSockets silent in both directions for a gap, then one message.
	for _, gap := range []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, total - time.Minute} {
		gap := gap
		if gap <= 0 || gap > total {
			continue
		}
		run("ws-idle-"+gap.String(), func() {
			c, _, err := websocket.Dial(ctx, "ws://"+up+"/ws?mode=idle", nil)
			if err != nil {
				logf("ws-idle %s: dial: %v", gap, err)
				return
			}
			time.Sleep(gap)
			if err := c.Write(ctx, websocket.MessageText, []byte("after "+gap.String())); err != nil {
				logf("ws-idle %s: DROPPED (write): %v", gap, err)
				return
			}
			rctx, rc := context.WithTimeout(ctx, 30*time.Second)
			defer rc()
			_, b, err := c.Read(rctx)
			if err != nil {
				logf("ws-idle %s: DROPPED (read): %v", gap, err)
				return
			}
			logf("ws-idle %s: HELD, got %q", gap, b)
		})
	}

	// 3. Long-polls of 50 s back to back, as Telegram's getUpdates.
	run("poll-50s", func() {
		deadline := time.Now().Add(total)
		ok, bad := 0, 0
		for time.Now().Before(deadline) {
			t := time.Now()
			r, err := http.Get("http://" + up + "/poll?hold=50s")
			if err != nil {
				bad++
				logf("poll-50s: FAILED after %s: %v", time.Since(t).Round(time.Second), err)
				time.Sleep(5 * time.Second)
				continue
			}
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
			ok++
		}
		logf("poll-50s: %d ok, %d failed", ok, bad)
	})

	// 4. One request held silent for 10 minutes and one for 30.
	for _, hold := range []time.Duration{10 * time.Minute, 30 * time.Minute} {
		hold := hold
		if hold > total {
			continue
		}
		run("poll-"+hold.String(), func() {
			cl := &http.Client{Timeout: hold + time.Minute}
			t := time.Now()
			r, err := cl.Get("http://" + up + "/poll?hold=" + hold.String())
			if err != nil {
				logf("poll %s: DROPPED after %s: %v", hold, time.Since(t).Round(time.Second), err)
				return
			}
			b, _ := io.ReadAll(r.Body)
			logf("poll %s: HELD, %q", hold, b)
		})
	}

	// 5. Discord's real gateway through its relay: HELLO, then heartbeats
	// with no IDENTIFY, to see whether the vendor's side holds too.
	run("discord", func() {
		c, _, err := websocket.Dial(ctx, "wss://gateway.discord.gg/?v=10&encoding=json", nil)
		if err != nil {
			logf("discord: dial: %v", err)
			return
		}
		c.SetReadLimit(1 << 20)
		_, b, err := c.Read(ctx)
		if err != nil {
			logf("discord: read hello: %v", err)
			return
		}
		var hello struct {
			Op int `json:"op"`
			D  struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			} `json:"d"`
		}
		_ = json.Unmarshal(b, &hello)
		logf("discord: HELLO op=%d heartbeat_interval=%dms", hello.Op, hello.D.HeartbeatInterval)
		iv := time.Duration(hello.D.HeartbeatInterval) * time.Millisecond
		start := time.Now()
		go func() {
			for {
				time.Sleep(iv)
				if c.Write(ctx, websocket.MessageText, []byte(`{"op":1,"d":null}`)) != nil {
					return
				}
			}
		}()
		acks := 0
		for time.Since(start) < total {
			_, b, err := c.Read(ctx)
			if err != nil {
				logf("discord: closed after %s, %d acks: %v", time.Since(start).Round(time.Second), acks, err)
				return
			}
			acks++
			if acks <= 2 || acks%20 == 0 {
				logf("discord: %d messages, last %s", acks, b)
			}
		}
		logf("discord: HELD %s unidentified, %d messages", total, acks)
	})

	// 6. Telegram's Bot API through its relay with a token that is not one:
	// TLS by name through the relay, and the shape of the refusal.
	run("telegram", func() {
		r, err := http.Get("https://api.telegram.org/bot0:not-a-token/getUpdates?timeout=5")
		if err != nil {
			logf("telegram: %v", err)
			return
		}
		b, _ := io.ReadAll(r.Body)
		logf("telegram: HTTP %d %s", r.StatusCode, b)
	})

	wg.Wait()
}
