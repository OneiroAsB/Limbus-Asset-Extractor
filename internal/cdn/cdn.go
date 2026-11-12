// Package cdn is the HTTP side of the pipeline: it fetches small text files
// (catalog_S1.hash) and large binary files (catalog_S1.bin, *.bundle) from
// the game's CDN the way the game client does - same headers, no
// compression - with retries, resumable downloads and size verification.
package cdn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default CDN endpoint and request headers.
const (
	DefaultBase          = "https://download.limbuscompanycdn.org"
	DefaultUnityVersion  = "6000.3.12f1"
	DefaultUserAgent     = "UnityPlayer/" + DefaultUnityVersion + " (UnityWebRequest/1.0, libcurl/2.0)"
	DefaultRequestedWith = "this_is_header_value"
)

// ErrSizeMismatch means a finished download had a different size than the
// catalog promised.
var ErrSizeMismatch = errors.New("downloaded size does not match the catalog")

// StatusError is a non-success HTTP status.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.URL, e.Code, http.StatusText(e.Code))
}

// retryable: 5xx, 408 and 429 are worth another try; other 4xx (404, 403...)
// will not fix themselves.
func (e *StatusError) retryable() bool {
	return e.Code >= 500 || e.Code == http.StatusRequestTimeout || e.Code == http.StatusTooManyRequests
}

// transient marks an error as worth retrying.
type transient struct{ err error }

func (t *transient) Error() string { return t.err.Error() }
func (t *transient) Unwrap() error { return t.err }

func retryable(err error) error { return &transient{err} }

func retryableIf(err error, yes bool) error {
	if yes {
		return retryable(err)
	}
	return err
}

// DefaultHeaders returns the headers the game sends.
func DefaultHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", DefaultUserAgent)
	h.Set("X-Unity-Version", DefaultUnityVersion)
	h.Set("X-Requested-With", DefaultRequestedWith)
	return h
}

// Client downloads files from the CDN. The zero value is not usable; use
// NewClient.
type Client struct {
	HTTP        *http.Client
	Header      http.Header   // sent with every request
	Retries     int           // extra attempts after the first one
	Backoff     time.Duration // base delay; doubles per attempt, capped at 30s
	IdleTimeout time.Duration // abort a transfer that receives nothing this long (0 = never)
}

// NewClient configures the game headers, retries, and timeouts.
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   16,
			// curl (and the game) don't ask for compressed bodies. Leaving
			// Go's automatic gzip on would also break Range resumption.
			DisableCompression: true,
		}},
		Header:      DefaultHeaders(),
		Retries:     3,
		Backoff:     time.Second,
		IdleTimeout: 60 * time.Second,
	}
}

// Result describes a finished Download.
type Result struct {
	Bytes   int64
	Cached  bool // file was already there with the expected size; nothing fetched
	Resumed bool // continued from a partial file left by an earlier attempt
}

// GetText fetches a small text resource and trims surrounding whitespace.
func (c *Client) GetText(ctx context.Context, url string) (string, error) {
	var out string
	err := c.withRetry(ctx, func() error {
		req, err := c.newRequest(ctx, url)
		if err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return retryable(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			se := &StatusError{URL: url, Code: resp.StatusCode}
			return retryableIf(se, se.retryable())
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return retryable(err)
		}
		out = strings.TrimSpace(string(body))
		return nil
	})
	return out, err
}

// Download fetches url into dest. If wantSize > 0 the finished file must be
// exactly that long, and a file already at dest with that size is reused
// without touching the network. Data is written to dest+".part" and renamed
// into place only when complete, so dest is never a half-written file. When
// wantSize > 0 a leftover .part is continued with an HTTP Range request;
// when the size is unknown the download always starts from scratch.
func (c *Client) Download(ctx context.Context, url, dest string, wantSize int64) (Result, error) {
	if wantSize > 0 {
		if fi, err := os.Stat(dest); err == nil && fi.Size() == wantSize {
			return Result{Bytes: wantSize, Cached: true}, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, err
	}
	part := dest + ".part"
	if wantSize <= 0 {
		// Without a known size we cannot tell whether a leftover .part
		// belongs to this version of the file (catalog_S1.bin has the same
		// name in every release), so never resume it.
		os.Remove(part)
	}

	var res Result
	err := c.withRetry(ctx, func() error {
		n, resumed, err := c.attempt(ctx, url, part, wantSize)
		if err != nil {
			return err
		}
		res = Result{Bytes: n, Resumed: resumed}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if err := os.Rename(part, dest); err != nil {
		return Result{}, err
	}
	return res, nil
}

// attempt makes one try at completing the .part file.
func (c *Client) attempt(ctx context.Context, url, part string, wantSize int64) (total int64, resumed bool, err error) {
	var have int64
	if fi, serr := os.Stat(part); serr == nil {
		have = fi.Size()
	}
	if wantSize > 0 {
		if have == wantSize { // an earlier run finished but died before the rename
			return have, true, nil
		}
		if have > wantSize {
			os.Remove(part)
			have = 0
		}
	}

	// reqCtx lets the idle watchdog abort a stalled transfer without
	// cancelling the caller's context.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, url)
	if err != nil {
		return 0, false, err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, false, retryable(err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		have = 0 // server ignored (or wasn't sent) Range: start over
		flags |= os.O_TRUNC
	case http.StatusPartialContent:
		if start, ok := rangeStart(resp.Header.Get("Content-Range")); !ok || start != have {
			os.Remove(part)
			return 0, false, retryable(fmt.Errorf("GET %s: server resumed at the wrong offset", url))
		}
		flags |= os.O_APPEND
		resumed = true
	case http.StatusRequestedRangeNotSatisfiable:
		os.Remove(part) // our partial file is unusable; next attempt starts clean
		return 0, false, retryable(&StatusError{URL: url, Code: resp.StatusCode})
	default:
		se := &StatusError{URL: url, Code: resp.StatusCode}
		return 0, false, retryableIf(se, se.retryable())
	}

	// Fail fast, before downloading hundreds of MB, when the server already
	// says the file isn't the size the catalog promised.
	if wantSize > 0 && resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 && resp.ContentLength != wantSize {
		return 0, false, fmt.Errorf("%w: server announces %d bytes, catalog says %d (%s)",
			ErrSizeMismatch, resp.ContentLength, wantSize, url)
	}

	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, false, err
	}
	var body io.Reader = resp.Body
	if c.IdleTimeout > 0 {
		wd := newWatchdog(c.IdleTimeout, cancel)
		defer wd.stop()
		body = &idleReader{r: resp.Body, wd: wd}
	}
	n, copyErr := io.Copy(f, body)
	closeErr := f.Close()
	if copyErr != nil {
		// Keep the .part: the next attempt resumes from it.
		if ctx.Err() == nil && reqCtx.Err() != nil {
			copyErr = fmt.Errorf("GET %s: no data for %s (stalled)", url, c.IdleTimeout)
		}
		return 0, false, retryable(copyErr)
	}
	if closeErr != nil {
		return 0, false, closeErr
	}

	total = have + n
	if wantSize > 0 && total != wantSize {
		os.Remove(part)
		// A clean fresh download of the wrong size will not change on retry
		// (stale catalog); a mismatch after resuming might, so try again.
		return 0, false, retryableIf(fmt.Errorf("%w: got %d bytes, catalog says %d (%s)",
			ErrSizeMismatch, total, wantSize, url), resumed)
	}
	return total, resumed, nil
}

func (c *Client) newRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// withRetry runs fn until it succeeds, fails with a non-transient error, the
// attempts are used up, or ctx ends.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	delay := c.Backoff
	for attempt := 0; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		err := fn()
		if err == nil {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		var tr *transient
		if !errors.As(err, &tr) {
			return err
		}
		if attempt >= c.Retries {
			return fmt.Errorf("%w (gave up after %d attempts)", tr.err, attempt+1)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if delay *= 2; delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
}

// rangeStart parses the first byte offset out of "bytes 100-999/1000".
func rangeStart(h string) (int64, bool) {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "bytes"))
	dash := strings.IndexByte(h, '-')
	if dash < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h[:dash]), 10, 64)
	return n, err == nil
}

type watchdog struct {
	t *time.Timer
	d time.Duration
}

func newWatchdog(d time.Duration, onIdle func()) *watchdog {
	return &watchdog{t: time.AfterFunc(d, onIdle), d: d}
}
func (w *watchdog) kick() { w.t.Reset(w.d) }
func (w *watchdog) stop() { w.t.Stop() }

type idleReader struct {
	r  io.Reader
	wd *watchdog
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.wd.kick()
	}
	return n, err
}
