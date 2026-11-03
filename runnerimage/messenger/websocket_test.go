package messenger

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsServer serves one WebSocket whose server side does serve.
func wsServer(t *testing.T, serve func(p *wsPeer)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := acceptWS(t, w, r); p != nil {
			serve(p)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
}

func TestTheWebSocketRefusesWhatRFC6455Forbids(t *testing.T) {
	for name, raw := range map[string][]byte{
		"a masked server frame":     {0x81, 0x81, 1, 2, 3, 4, 'x'},
		"a reserved bit":            {0xC1, 0x01, 'x'},
		"a fragmented control":      {0x09, 0x00},
		"a continuation of nothing": {0x80, 0x01, 'x'},
	} {
		url := wsServer(t, func(p *wsPeer) {
			_, _ = p.c.Write(raw)
			time.Sleep(200 * time.Millisecond)
		})
		ws, err := dialWebSocket(t.Context(), url, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ws.read(5 * time.Second); err == nil {
			t.Errorf("%s was read", name)
		}
		ws.close()
	}
}

func TestTheWebSocketAnswersAPingAndAPongKeepsItOpen(t *testing.T) {
	got := make(chan bool, 1)
	url := wsServer(t, func(p *wsPeer) {
		p.frame(wsPing, []byte("hi"))
		var h [2]byte
		_, _ = p.br.Read(h[:])
		got <- h[0]&0x0f == wsPong
		// Pongs and nothing else for longer than the client's idle bound.
		for range 4 {
			time.Sleep(100 * time.Millisecond)
			p.frame(wsPong, nil)
		}
		p.text([]byte("after the pongs"))
		time.Sleep(200 * time.Millisecond)
	})
	ws, err := dialWebSocket(t.Context(), url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.close()
	b, err := ws.read(250 * time.Millisecond)
	if err != nil || string(b) != "after the pongs" {
		t.Fatalf("read = %q, %v, want the message after a quiet spell of pongs", b, err)
	}
	if !<-got {
		t.Fatal("the ping was not answered with a pong")
	}
}
