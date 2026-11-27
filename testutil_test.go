package main

import (
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed+1)) }

// fixedGame builds a deterministic board: ember starts.
// idx 0-8 ember, 9-16 tide, 17-23 bystander, 24 burned.
func fixedGame() *Game {
	g := &Game{Starter: Ember, Turn: Ember, Cards: make([]Card, BoardSize)}
	for i := range g.Cards {
		c := ColorBystander
		switch {
		case i < 9:
			c = ColorEmber
		case i < 17:
			c = ColorTide
		case i == 24:
			c = ColorBurned
		}
		g.Cards[i] = Card{Word: "word" + string(rune('a'+i)), Color: c}
	}
	return g
}

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

// handlerTransport serves requests straight from the handler (no sockets).
type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func newClient(t *testing.T, h http.Handler) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: "http://deaddrop.test", hc: &http.Client{
		Transport:     handlerTransport{h},
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) post(path string, form url.Values) (int, string) {
	c.t.Helper()
	return c.postHeaders(path, form, map[string]string{"Origin": c.base})
}

func (c *client) postHeaders(path string, form url.Values, hdrs map[string]string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if loc := resp.Header.Get("Location"); loc != "" {
		return resp.StatusCode, loc
	}
	return resp.StatusCode, string(b)
}

func (c *client) get(path string) (int, string) {
	c.t.Helper()
	resp, err := c.hc.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var versionRe = regexp.MustCompile(`state\?v=(\d+)`)

func newTestApp(t *testing.T) (*Store, *App, http.Handler) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	app, err := NewApp(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, app, app.Routes()
}
