package cdn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient() *Client {
	c := NewClient()
	c.Backoff = time.Millisecond // keep retry tests fast
	c.IdleTimeout = 2 * time.Second
	return c
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestGameHeadersAreSentOnEveryRequest(t *testing.T) {
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		w.Write([]byte("abc\n"))
	}))
	defer srv.Close()

	c := testClient()
	c.Header.Set("X-Requested-With", "custom")
	if _, err := c.GetText(context.Background(), srv.URL+"/x.hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Download(context.Background(), srv.URL+"/y.bin", filepath.Join(t.TempDir(), "y.bin"), 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(got))
	}
	for i, h := range got {
		if h.Get("User-Agent") != DefaultUserAgent {
			t.Errorf("req %d: User-Agent = %q", i, h.Get("User-Agent"))
		}
		if h.Get("X-Unity-Version") != DefaultUnityVersion {
			t.Errorf("req %d: X-Unity-Version = %q", i, h.Get("X-Unity-Version"))
		}
		if h.Get("X-Requested-With") != "custom" {
			t.Errorf("req %d: X-Requested-With = %q (override ignored)", i, h.Get("X-Requested-With"))
		}
		if h.Get("Accept-Encoding") != "" {
			t.Errorf("req %d: must not ask for compression, got %q", i, h.Get("Accept-Encoding"))
		}
	}
}

func TestGetTextTrims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("  91d7cade706fdadb73e61f4dd34d7538\r\n"))
	}))
	defer srv.Close()
	got, err := testClient().GetText(context.Background(), srv.URL)
	if err != nil || got != "91d7cade706fdadb73e61f4dd34d7538" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestDownloadOKCachedAndNoPartLeftBehind(t *testing.T) {
	data := payload(10_000)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "sub", "a.bundle")
	c := testClient()
	res, err := c.Download(context.Background(), srv.URL, dest, int64(len(data)))
	if err != nil || res.Cached || res.Bytes != int64(len(data)) {
		t.Fatalf("first download: %+v err=%v", res, err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part should be gone after success")
	}

	res, err = c.Download(context.Background(), srv.URL, dest, int64(len(data)))
	if err != nil || !res.Cached {
		t.Fatalf("second download should be a cache hit: %+v err=%v", res, err)
	}
	if hits != 1 {
		t.Errorf("cache hit must not touch the network, server saw %d requests", hits)
	}
}

func TestDownloadSizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload(100))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "a.bundle")
	_, err := testClient().Download(context.Background(), srv.URL, dest, 999)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s must not exist after a size mismatch", p)
		}
	}
}

func TestSizeMismatchOnFreshDownloadIsNotRetried(t *testing.T) {
	// A stale catalog will not get better by re-downloading a 400 MB file.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(payload(100))
	}))
	defer srv.Close()
	_, err := testClient().Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "a"), 999)
	if !errors.Is(err, ErrSizeMismatch) || hits != 1 {
		t.Fatalf("hits=%d err=%v", hits, err)
	}
}

func TestRetriesServerErrorsThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	got, err := testClient().GetText(context.Background(), srv.URL)
	if err != nil || got != "ok" || hits != 3 {
		t.Fatalf("got=%q hits=%d err=%v", got, hits, err)
	}
}

func TestDoesNotRetryNotFound(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := testClient().GetText(context.Background(), srv.URL)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("want 404 StatusError, got %v", err)
	}
	if hits != 1 {
		t.Errorf("404 must not be retried; server saw %d requests", hits)
	}
}

func TestGivesUpAfterRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := testClient()
	c.Retries = 2
	if _, err := c.GetText(context.Background(), srv.URL); err == nil {
		t.Fatal("expected failure")
	}
	if hits != 3 {
		t.Errorf("1 try + 2 retries = 3 requests, got %d", hits)
	}
}

// rangeServer serves data with real Range support and records the Range
// header of each request.
func rangeServer(data []byte, ranges *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ranges = append(*ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "x.bundle", time.Time{}, bytes.NewReader(data))
	}))
}

func TestResumesFromPartFile(t *testing.T) {
	data := payload(50_000)
	var ranges []string
	srv := rangeServer(data, &ranges)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "a.bundle")
	if err := os.WriteFile(dest+".part", data[:20_000], 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data)))
	if err != nil || !res.Resumed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if want := "bytes=20000-"; len(ranges) != 1 || ranges[0] != want {
		t.Errorf("Range headers = %v, want [%s]", ranges, want)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("resumed file is corrupt")
	}
}

func TestServerIgnoringRangeRestartsFromScratch(t *testing.T) {
	data := payload(30_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) // always 200, never 206
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "a.bundle")
	os.WriteFile(dest+".part", []byte("garbage-from-an-old-attempt"), 0o644)
	if _, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("stale partial data leaked into the result")
	}
}

func TestPartLargerThanExpectedIsDiscarded(t *testing.T) {
	data := payload(1000)
	var ranges []string
	srv := rangeServer(data, &ranges)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "a.bundle")
	os.WriteFile(dest+".part", payload(5000), 0o644)
	if _, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
}

func TestCompletePartFileIsJustRenamed(t *testing.T) {
	data := payload(4000)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "a.bundle")
	os.WriteFile(dest+".part", data, 0o644)
	if _, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Errorf("a finished .part needs no request, server saw %d", hits)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
}

func TestInterruptedTransferResumesOnRetry(t *testing.T) {
	data := payload(60_000)
	var hits int32
	var secondRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// Promise everything, deliver 40%, then drop the connection.
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:24_000])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		secondRange = r.Header.Get("Range")
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "a.bundle")
	res, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed || secondRange != "bytes=24000-" {
		t.Errorf("expected resume from 24000, got resumed=%v range=%q", res.Resumed, secondRange)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("file corrupt after resume")
	}
}

func TestWrongResumeOffsetIsRejected(t *testing.T) {
	data := payload(10_000)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 && r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "a.bundle")
	os.WriteFile(dest+".part", data[:3000], 0o644)
	if _, err := testClient().Download(context.Background(), srv.URL, dest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
		t.Fatal("wrong-offset 206 corrupted the file")
	}
}

func TestUnknownSizeNeverResumesStalePart(t *testing.T) {
	// catalog_S1.bin has the same name in every release: a .part left over
	// from an older catalog must not be glued onto the new one.
	fresh := payload(2000)
	var ranges []string
	srv := rangeServer(fresh, &ranges)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "catalog_S1.bin")
	os.WriteFile(dest+".part", bytes.Repeat([]byte{0xEE}, 700), 0o644)
	if _, err := testClient().Download(context.Background(), srv.URL, dest, 0); err != nil {
		t.Fatal(err)
	}
	if ranges[0] != "" {
		t.Errorf("must not send Range for an unknown-size download, sent %q", ranges[0])
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, fresh) {
		t.Fatal("stale part leaked into the new catalog")
	}
}

func TestStalledTransferIsAbortedByIdleWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		select { // never finish
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()

	c := testClient()
	c.IdleTimeout = 150 * time.Millisecond
	c.Retries = 0
	start := time.Now()
	_, err := c.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "a"), 1000)
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("watchdog too slow: %v", time.Since(start))
	}
}

func TestContextCancelStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient()
	c.Backoff = time.Hour // would hang if cancellation were ignored
	c.Retries = 5
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetText(ctx, srv.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cancellation not honoured promptly: %v", time.Since(start))
	}
}

func TestRangeStart(t *testing.T) {
	for in, want := range map[string]int64{"bytes 100-999/1000": 100, "bytes 0-9/10": 0, " bytes  5-6/*": 5} {
		if got, ok := rangeStart(in); !ok || got != want {
			t.Errorf("rangeStart(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "bytes */1000", "garbage"} {
		if _, ok := rangeStart(bad); ok {
			t.Errorf("rangeStart(%q) should fail", bad)
		}
	}
}
