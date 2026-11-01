package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	jar []*http.Cookie
}

func (c *client) do(method, path, body string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	c.jar = append(c.jar, res.Cookies()...)
	var out map[string]any
	if res.StatusCode != http.StatusNoContent {
		json.NewDecoder(res.Body).Decode(&out)
	}
	res.Body.Close()
	return res, out
}

// snapshot reads the first SSE message for this client.
func (c *client) snapshot(room string) map[string]any {
	req, _ := http.NewRequest("GET", c.srv.URL+"/api/rooms/"+room+"/events", nil)
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		c.t.Fatalf("events: %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			var v map[string]any
			json.Unmarshal([]byte(line[6:]), &v)
			return v
		}
	}
	c.t.Fatal("no snapshot")
	return nil
}

func TestRoomFlow(t *testing.T) {
	s := New(Options{SecureCookie: false})
	srv := httptest.NewServer(s)
	defer srv.Close()
	a := &client{t: t, srv: srv}
	b := &client{t: t, srv: srv}
	c := &client{t: t, srv: srv}

	res, out := a.do("POST", "/api/rooms", "")
	if res.StatusCode != 201 {
		t.Fatalf("create: %d", res.StatusCode)
	}
	id := out["id"].(string)
	path := "/api/rooms/" + id

	if res, out := a.do("POST", path+"/join", ""); res.StatusCode != 200 || out["seat"].(float64) != 0 {
		t.Fatalf("a join: %d %v", res.StatusCode, out)
	}
	if v := a.snapshot(id); v["phase"] != "waiting" {
		t.Fatalf("phase before b: %v", v["phase"])
	}
	if res, _ := a.do("POST", path+"/join", ""); res.StatusCode != 200 {
		t.Fatalf("a rejoin: %d", res.StatusCode)
	}
	if res, out := b.do("POST", path+"/join", ""); res.StatusCode != 200 || out["seat"].(float64) != 1 {
		t.Fatalf("b join: %d %v", res.StatusCode, out)
	}
	if res, _ := c.do("POST", path+"/join", ""); res.StatusCode != 409 {
		t.Fatalf("third player: %d", res.StatusCode)
	}
	if res, _ := c.do("POST", path+"/flip", `{"seq":1,"index":0}`); res.StatusCode != 403 {
		t.Fatalf("unseated flip: %d", res.StatusCode)
	}

	v := a.snapshot(id)
	if v["phase"] != "playing" || v["turn"].(float64) != 0 {
		t.Fatalf("snapshot: %v", v)
	}
	seq := int(v["seq"].(float64))
	// Stale seq is rejected, correct seq accepted, wrong seat rejected.
	if res, _ := a.do("POST", path+"/flip", `{"seq":0,"index":0}`); res.StatusCode != 409 {
		t.Fatalf("stale: %d", res.StatusCode)
	}
	if res, _ := b.do("POST", path+"/flip", `{"seq":`+itoa(seq)+`,"index":0}`); res.StatusCode != 409 {
		t.Fatalf("b out of turn: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", path+"/region", `{"seq":`+itoa(seq)+`,"region":2}`); res.StatusCode != 204 {
		t.Fatalf("move: %d", res.StatusCode)
	}
	v = b.snapshot(id)
	if v["region"].(float64) != 2 || int(v["seq"].(float64)) != seq+1 {
		t.Fatalf("b should follow region: %v", v)
	}
	seq++
	if res, _ := a.do("POST", path+"/flip", `{"seq":`+itoa(seq)+`,"index":30}`); res.StatusCode != 204 {
		t.Fatalf("flip: %d", res.StatusCode)
	}
	v = a.snapshot(id)
	cards := v["cards"].([]any)
	up := cards[30].(map[string]any)
	if up["state"] != "up" || up["rank"].(float64) < 1 {
		t.Fatalf("card 30: %v", up)
	}
	for i, c := range cards {
		if i != 30 && c.(map[string]any)["state"] != "down" {
			t.Fatalf("card %d leaked: %v", i, c)
		}
	}
	if res, _ := a.do("POST", path+"/rematch", `{"seq":`+itoa(seq+1)+`}`); res.StatusCode != 409 {
		t.Fatalf("rematch mid-game: %d", res.StatusCode)
	}
	if res, _ := a.do("POST", "/api/rooms/nope/join", ""); res.StatusCode != 404 {
		t.Fatalf("missing room: %d", res.StatusCode)
	}
}

func TestSweepClosesIdleRooms(t *testing.T) {
	rs := newRooms(10)
	r, err := rs.create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n := rs.sweep(time.Now()); n != 0 {
		t.Fatalf("fresh room swept: %d", n)
	}
	if n := rs.sweep(time.Now().Add(unjoinedTTL + time.Second)); n != 1 {
		t.Fatalf("unjoined room kept: %d", n)
	}
	if _, _, err := r.join(""); err != errRoomClosed {
		t.Fatalf("join closed room: %v", err)
	}
	if _, _, err := r.subscribe(); err != errRoomClosed {
		t.Fatalf("subscribe closed room: %v", err)
	}
}

func TestCreateRateLimit(t *testing.T) {
	rs := newRooms(1000)
	now := time.Now()
	for i := 0; i < 30; i++ {
		if _, err := rs.create(now); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := rs.create(now); err == nil {
		t.Fatal("31st room within a second should be refused")
	}
	if _, err := rs.create(now.Add(2 * time.Second)); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
